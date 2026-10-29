package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestJSONCLIAuthenticationExamples(t *testing.T) {
	python := testPython(t)
	root := t.TempDir()
	packageDir := filepath.Join(root, "crunchyroll_video_analyzer")
	if err := os.Mkdir(packageDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "credential_store.py"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	toolDir, toolName := t.TempDir(), "ffmpeg"
	if runtime.GOOS == "windows" {
		toolName += ".exe"
	}
	if err := os.WriteFile(filepath.Join(toolDir, toolName), []byte("not an executable"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct{ mode, file, steps, reason string }{
		{"session_cached", "cached-session", "checking_session validating_session authenticated", ""},
		{"session_expired", "renewed-session", "checking_session refreshing_session validating_session saving_session authenticated", ""},
		{"session_unsupported", "unsupported-renewal", "checking_session refreshing_session", "session_refresh_unsupported"},
		{"session_locked", "locked-store", "checking_session", "credential_locked"},
		{"session_rejected", "rejected-cookie", "validating_session", "session_rejected"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "work-item.mp4")
			if err := os.WriteFile(output, []byte("existence-only fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--progress-format", "json", "--url", "https://www.crunchyroll.com/watch/TEST", "--output-format", "mp4", "--output-dir", dir, "--filename", "work-item", "--cva-root", root, "--cva-python", python}
			if tc.mode == "session_rejected" {
				args = append(args, "--etp-rt", "private-cookie")
			}
			records, stderr := runJSONProcess(t, tc.mode, args, nil)
			assertAuthSteps(t, records, tc.steps)
			last := records[len(records)-1]
			if tc.reason != "" {
				if last["event"] != "failed" || last["reason"] != tc.reason || last["phase"] != "authenticating" {
					t.Fatalf("wrong terminal: %v", last)
				}
			} else if last["event"] != "skipped_existing" || last["output_path"] != output {
				t.Fatalf("wrong success: %v", last)
			}
			if strings.Contains(stderr, "private-") {
				t.Fatal("private helper or cookie data leaked to stderr")
			}
			// Documentation examples are captured from the real CLI. Normalize
			// only variable timing and the disposable OS-specific output path.
			var normalized bytes.Buffer
			encoder := json.NewEncoder(&normalized)
			for _, record := range records {
				record["elapsed_ms"] = 0
				if _, ok := record["output_path"]; ok {
					record["output_path"] = "/downloads/stage2/Example/Season 01/work-item.mp4"
				}
				if err := encoder.Encode(record); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join("examples", "progress", tc.file+".jsonl")
			if os.Getenv("CRDL_UPDATE_PROGRESS_EXAMPLES") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, normalized.Bytes(), 0644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), normalized.Bytes()) {
				t.Fatalf("progress example %s differs from CLI output; inspect the change before regenerating with CRDL_UPDATE_PROGRESS_EXAMPLES=1: %v\n%s", path, err, normalized.String())
			}
		})
	}
}

func assertAuthSteps(t *testing.T, records []map[string]any, want string) {
	t.Helper()
	var steps []string
	for _, e := range records {
		if step, ok := e["auth_step"].(string); ok {
			if e["event"] != "phase" || e["phase"] != "authenticating" || authStepMessages[step] == "" || e["display_message"] != authStepMessages[step] {
				t.Fatalf("invalid authentication step: %v", e)
			}
			steps = append(steps, step)
		}
		if e["event"] == "attempt_failed" || e["event"] == "retry_wait" || (e["event"] == "attempt_started" && e["attempt"] != float64(1)) {
			t.Fatal("authentication changed download retry state")
		}
	}
	if got := strings.Join(steps, " "); got != want {
		t.Fatalf("authentication steps = %q, want %q", got, want)
	}
}

func TestAuthenticationFailureReasons(t *testing.T) {
	for reason, message := range cvaStatusMessages {
		t.Run(reason, func(t *testing.T) {
			buf := captureEvents(t)
			// Errors survive ordinary wrapping and cleanup without losing their
			// authentication phase or reflecting private error text into JSON.
			err := fmt.Errorf("private-cookie-secret: %w", cvaStatusError(reason))
			err = operationFailure(err, "download_failed", "cleanup")
			exit := events.finish(err, "mp4")
			records := checkProtocol(t, buf.Bytes(), exit)
			last := records[len(records)-1]
			if last["reason"] != reason || last["display_message"] != message || last["phase"] != "authenticating" {
				t.Fatalf("lost authentication detail: %v", last)
			}
			if strings.Contains(buf.String(), "private-cookie-secret") {
				t.Fatal("raw failure text leaked")
			}
		})
	}
}

func TestAuthenticationUnknownDetailsStayPrivate(t *testing.T) {
	for _, err := range []error{
		cvaStatusError("private-cookie-secret"),
		&operationError{code: "authentication_failed", phase: "authenticating", reason: "private-cookie-secret", err: errors.New("private-cookie-secret")},
		&operationError{code: "authentication_failed", phase: "authenticating", reason: "credential_locked", err: errors.New("private-cookie-secret")},
	} {
		buf := captureEvents(t)
		reportAuthStep("private-cookie-secret")
		exit := events.finish(operationFailure(err, "authentication_failed", "authenticating"), "mp4")
		records := checkProtocol(t, buf.Bytes(), exit)
		last := records[len(records)-1]
		if _, exists := last["reason"]; exists || last["display_message"] != failureMessages["authentication_failed"] || strings.Contains(buf.String(), "private-") {
			t.Fatalf("untrusted detail was rendered: %v", last)
		}
	}
}

func TestAuthenticationTextMessages(t *testing.T) {
	previousEvents, previousStderr := events, os.Stderr
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { events, os.Stderr = previousEvents, previousStderr; file.Close() })
	events, os.Stderr = nil, file
	reportAuthStep("checking_session")
	reportAuthStep("validating_session")
	reportAuthStep("authenticated")
	reportAuthStep("private-cookie-secret")
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "Checking saved sign-in...\nValidating sign-in...\nSigned in successfully.\n" {
		t.Fatalf("unexpected console messages: %q (%v)", data, err)
	}
}

func TestJSONAuthenticationDuringDownload(t *testing.T) {
	for _, mode := range []string{"success", "failure", "parallel_phase"} {
		t.Run(mode, func(t *testing.T) {
			fail := mode == "failure"
			buf := captureEvents(t)
			events.startAttempt()
			events.setPhase("downloading")
			track := trackIdentity{id: "video:0", kind: "video"}
			events.trackStarted(track)
			events.progress(track, 25, 100, true, false, 1)
			oldProvider, oldToken := accessTokenProvider, token
			t.Cleanup(func() { accessTokenProvider, token = oldProvider, oldToken })
			token = "private-old-token"
			p := &sessionTokenProvider{
				now:  time.Now,
				read: func() (*storedSession, error) { return nil, nil },
				refresh: func(bool) (*storedSession, error) {
					if fail {
						return nil, cvaStatusError("session_refresh_unsupported")
					}
					return &storedSession{Cookie: "private-cookie", DeviceID: "private-device"}, nil
				},
				exchange: func(s storedSession) (string, storedSession, error) { return "private-new-token", s, nil },
				save: func(storedSession) error {
					if mode == "parallel_phase" {
						events.setPhase("decrypting")
					}
					return nil
				},
			}
			accessTokenProvider = p.token
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer private-new-token" {
					w.WriteHeader(http.StatusUnauthorized)
				}
			}))
			defer server.Close()
			req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
			req.Header.Set("Authorization", "Bearer private-old-token")
			resp, err := DoRequest(req)
			if (err != nil) != fail {
				t.Fatalf("unexpected refresh result: %v", err)
			}
			if err == nil {
				resp.Body.Close()
				wantPhase := "downloading"
				if mode == "parallel_phase" {
					wantPhase = "decrypting"
				}
				if events.currentPhase() != wantPhase {
					t.Fatal("latest work phase was not restored")
				}
				events.progress(track, 50, 100, true, false, 1)
				events.output(filepath.Join(t.TempDir(), "episode.mp4"), false)
			}
			exit := events.finish(err, "mp4")
			records := checkProtocol(t, buf.Bytes(), exit)
			steps := "checking_session refreshing_session"
			if !fail {
				steps += " validating_session saving_session authenticated"
			} else if last := records[len(records)-1]; last["reason"] != "session_refresh_unsupported" || last["phase"] != "authenticating" || last["attempt"] != float64(1) {
				t.Fatalf("wrong renewal failure: %v", last)
			}
			assertAuthSteps(t, records, steps)
			if strings.Contains(buf.String(), "private-") {
				t.Fatal("session data leaked into download progress")
			}
		})
	}
}
