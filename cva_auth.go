package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

//go:embed cva_auth.py
var cvaAuthScript string

// Both initial authentication and 401 recovery use the selected provider.
var accessTokenProvider = func() (string, error) { return GetAccessToken(*etpRt) }

type cvaAuthOptions struct {
	Root, Python, SourceURL string
	SaveCookie              bool
}

func findCVARoot(explicit string) (string, error) {
	candidates := []string{explicit}
	if explicit == "" {
		if configured := os.Getenv("CRUNCHYROLL_CVA_ROOT"); configured != "" {
			candidates = []string{configured}
		} else {
			candidates = []string{filepath.Join("..", "Crunchyroll Video Analyzer")}
			if executable, err := os.Executable(); err == nil {
				candidates = append(candidates, filepath.Join(filepath.Dir(executable), "..", "Crunchyroll Video Analyzer"))
			}
		}
	}
	for _, candidate := range candidates {
		root, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, "crunchyroll_video_analyzer", "credential_store.py")); err == nil && !info.IsDir() {
			return root, nil
		}
	}
	return "", errors.New("CVA was not found; set --cva-root to your Crunchyroll Video Analyzer folder")
}

func newAccessTokenProvider(cookie string, options cvaAuthOptions) (func() (string, error), error) {
	if cookie != "" && !options.SaveCookie {
		return func() (string, error) { return GetAccessToken(cookie) }, nil
	}
	options, err := resolveCVAOptions(options)
	if err != nil {
		return nil, err
	}
	var explicit *storedSession
	if cookie != "" {
		explicit = &storedSession{Cookie: cookie, DeviceID: deviceId}
	}
	return newSessionTokenProvider(options, explicit).token, nil
}

func resolveCVAOptions(options cvaAuthOptions) (cvaAuthOptions, error) {
	root, err := findCVARoot(options.Root)
	if err != nil {
		return options, err
	}
	options.Root = root
	if options.Python == "" {
		options.Python = os.Getenv("CRUNCHYROLL_CVA_PYTHON")
	}
	if options.Python == "" {
		options.Python = "python"
		if runtime.GOOS != "windows" {
			if _, err := exec.LookPath("python3"); err == nil {
				options.Python = "python3"
			}
		}
	}
	if _, err := exec.LookPath(options.Python); err != nil {
		return options, errors.New("Python was not found; set --cva-python to the Python executable used by CVA")
	}
	return options, nil
}

// boundedOutput prevents unexpected child output from growing without limit.
// Its contents must never be included in errors or printed.
type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64*1024 {
		return 0, errors.New("authentication helper output exceeded its limit")
	}
	return b.Buffer.Write(p)
}

func getCVAToken(ctx context.Context, options cvaAuthOptions) (string, error) {
	data, err := runCVAHelper(ctx, options, "token")
	if err != nil {
		return "", err
	}
	return decodeCVAToken(data)
}

func checkCVACredentials(options cvaAuthOptions) (string, error) {
	options, err := resolveCVAOptions(options)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	data, err := runCVAHelper(ctx, options, "credentials-check")
	if err != nil {
		return "", err
	}
	return decodeCVACredentialCheck(data)
}

func runCVAHelper(ctx context.Context, options cvaAuthOptions, operation string) ([]byte, error) {
	return runCVAHelperRequest(ctx, options, map[string]any{"operation": operation})
}

func runCVAHelperRequest(ctx context.Context, options cvaAuthOptions, request map[string]any) ([]byte, error) {
	request["source_url"] = options.SourceURL
	input, err := json.Marshal(request)
	if err != nil {
		return nil, errors.New("invalid authentication helper request")
	}
	// The embedded helper contains no credentials. Account/password stay inside
	// Python; bearer/session exchange uses only the child's private pipes.
	cmd := exec.CommandContext(ctx, options.Python, "-I", "-B", "-c", cvaAuthScript, options.Root)
	cmd.Stdin = bytes.NewReader(input)
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, errors.New("CVA authentication helper timed out or was cancelled; session storage may need to be checked")
	}
	if err != nil {
		return nil, errors.New("CVA authentication helper could not run; check --cva-python and the CVA dependencies")
	}
	return output.Bytes(), nil
}

func decodeCVACredentialCheck(data []byte) (string, error) {
	var result struct {
		Status  string `json:"status"`
		Runtime string `json:"runtime"`
		Backend string `json:"backend"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", errors.New("CVA credential check returned an invalid response")
	}
	if result.Status != "credentials_ok" {
		return "", cvaStatusError(result.Status)
	}
	if result.Backend == "os_keyring" && (result.Runtime == "windows_desktop" || result.Runtime == "posix_desktop") {
		return "OS keyring", nil
	}
	if result.Backend == "container_vault" && result.Runtime == "container" {
		return "container vault", nil
	}
	return "", errors.New("CVA credential check returned an invalid store selection")
}

func decodeCVAToken(data []byte) (string, error) {
	var result struct {
		Status string `json:"status"`
		Token  string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", errors.New("CVA authentication returned an invalid response")
	}
	if result.Status == "ok" && result.Token != "" {
		// A bearer goes into an HTTP header. Reject whitespace/control bytes.
		for _, char := range result.Token {
			if char <= ' ' || char >= 127 {
				return "", errors.New("CVA authentication returned an invalid access token")
			}
		}
		return result.Token, nil
	}
	return "", cvaStatusError(result.Status)
}

// These allowlisted statuses are also the optional JSON failure reasons.
// Keep messages static: helper output must never become UI text.
var cvaStatusMessages = map[string]string{
	"session_rejected":            "Crunchyroll rejected the session cookie. Supply a fresh cookie or renew sign-in before retrying.",
	"session_invalid":             "the stored or returned downloader session is invalid; replace it with a valid cookie using --etp-rt and --save-cookie",
	"session_unavailable":         "sign-in did not provide a usable downloader session cookie",
	"session_refresh_unsupported": "A usable saved sign-in is unavailable. Automatic renewal requires Windows or an active CVA container authentication connection. Run through CVA or supply a fresh cookie with --etp-rt and --save-cookie.",
	"dependencies":                "CVA modules or dependencies are unavailable; check --cva-root and --cva-python",
	"credentials_missing":         "no CVA credentials are stored; add them using CVA's credential manager",
	"credential_config":           "invalid CVA credential configuration; check CVA_CREDENTIAL_BACKEND, CVA_CONTAINER, CVA_CREDENTIAL_VAULT_PATH and CVA_CREDENTIAL_KEY_PATH",
	"credential_unavailable":      "the selected CVA credential store is unavailable; check its dependencies, mounted files, ownership and permissions",
	"credential_locked":           "the CVA credential vault is locked; check the mounted unlock key using CVA's vault documentation",
	"credential_corrupt":          "the CVA credential vault is corrupt or unsupported; repair or restore it through CVA before retrying",
	"credential_store":            "CVA credentials could not be accessed; check the selected store using CVA's credential manager",
	"bad_credentials":             "Crunchyroll rejected the stored credentials; update them using CVA's credential manager",
	"interaction_required":        "Crunchyroll requires interactive sign-in; use the browser cookie option (--etp-rt) for this run",
	"verification_required":       "Crunchyroll requires human verification; complete sign-in in your browser and use --etp-rt for this run",
	"source":                      "CVA authentication requires a valid HTTPS Crunchyroll series or watch URL",
	"configuration":               "CVA could not load Crunchyroll's sign-in configuration",
	"runtime":                     "CVA sign-in could not complete; check the Windows Chrome runtime or active CVA container authentication connection",
	"browser_runtime_unsupported": "CVA credential selection succeeded, but this bridge's browser sign-in runtime requires Windows; use --etp-rt on this platform",
	"network_error":               "CVA sign-in encountered a network error",
	"site_error":                  "Crunchyroll sign-in is temporarily unavailable",
	"auth_flow_changed":           "CVA could not recognize Crunchyroll's current sign-in flow",
	"profile_error":               "CVA could not use the downloader's dedicated browser profile",
}

func cvaFailureDetail(status string) (code, message string, known bool) {
	if message, ok := cvaStatusMessages[status]; ok {
		code := "authentication_failed"
		if strings.HasPrefix(status, "credential") {
			code = "credential_storage_failed"
		}
		if status == "session_invalid" {
			code = "credential_storage_failed"
		}
		if status == "dependencies" || status == "configuration" || status == "source" {
			code = "sign_in_configuration_failed"
		}
		return code, message, true
	}
	return "", "", false
}

func cvaStatusError(status string) error {
	if code, message, known := cvaFailureDetail(status); known {
		return &operationError{code: code, phase: "authenticating", reason: status, err: errors.New(message)}
	}
	// Never reflect helper data (including unknown statuses) into diagnostics.
	return errors.New("CVA authentication failed; the helper returned an unsupported result")
}
