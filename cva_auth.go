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
	"time"
)

//go:embed cva_auth.py
var cvaAuthScript string

// Both initial authentication and 401 recovery use the selected provider.
var accessTokenProvider = func() (string, error) { return GetAccessToken(*etpRt) }

type cvaAuthOptions struct {
	Root, Python, SourceURL string
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
	if cookie != "" {
		return func() (string, error) { return GetAccessToken(cookie) }, nil
	}
	if runtime.GOOS != "windows" {
		return nil, errors.New("CVA keyring sign-in requires Windows; use --etp-rt for the original cookie authentication on other systems")
	}
	root, err := findCVARoot(options.Root)
	if err != nil {
		return nil, err
	}
	options.Root = root
	if options.Python == "" {
		options.Python = os.Getenv("CRUNCHYROLL_CVA_PYTHON")
	}
	if options.Python == "" {
		options.Python = "python"
	}
	if _, err := exec.LookPath(options.Python); err != nil {
		return nil, errors.New("Python was not found; set --cva-python to the Python executable used by CVA")
	}
	return func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return getCVAToken(ctx, options)
	}, nil
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
	input, _ := json.Marshal(map[string]string{"source_url": options.SourceURL})
	// The embedded helper contains no credentials. Account/password stay inside
	// Python; the bearer is returned only through the child's private pipe.
	cmd := exec.CommandContext(ctx, options.Python, "-I", "-B", "-c", cvaAuthScript, options.Root)
	cmd.Stdin = bytes.NewReader(input)
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", errors.New("CVA authentication timed out or was cancelled; no credentials were changed")
	}
	if err != nil {
		return "", errors.New("CVA authentication helper could not run; check --cva-python and the CVA dependencies")
	}
	return decodeCVAToken(output.Bytes())
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
	messages := map[string]string{
		"dependencies":          "CVA modules or dependencies are unavailable; check --cva-root and --cva-python",
		"credentials_missing":   "no CVA credentials are stored; add them using CVA's credential manager",
		"credential_store":      "CVA credentials could not be read; check the OS keyring using CVA's credential manager",
		"bad_credentials":       "Crunchyroll rejected the stored credentials; update them using CVA's credential manager",
		"interaction_required":  "Crunchyroll requires interactive sign-in; use the browser cookie option (--etp-rt) for this run",
		"verification_required": "Crunchyroll requires human verification; complete sign-in in your browser and use --etp-rt for this run",
		"source":                "CVA authentication requires a valid HTTPS Crunchyroll series or watch URL",
		"configuration":         "CVA could not load Crunchyroll's sign-in configuration",
		"runtime":               "CVA could not start or close the dedicated Chrome runtime; check CVA's Windows/Chrome requirements",
		"network_error":         "CVA sign-in encountered a network error",
		"site_error":            "Crunchyroll sign-in is temporarily unavailable",
		"auth_flow_changed":     "CVA could not recognize Crunchyroll's current sign-in flow",
		"profile_error":         "CVA could not use the downloader's dedicated browser profile",
	}
	if message, ok := messages[result.Status]; ok {
		return "", errors.New(message)
	}
	// Never reflect helper data (including unknown statuses) into diagnostics.
	return "", errors.New("CVA authentication failed; no credentials were changed")
}
