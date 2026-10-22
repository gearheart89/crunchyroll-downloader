package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeCVAToken(t *testing.T) {
	for _, tc := range []struct {
		name, input, want, message string
	}{
		{"success", `{"status":"ok","access_token":"fixture-token"}`, "fixture-token", ""},
		{"missing", `{"status":"credentials_missing"}`, "", "no CVA credentials"},
		{"incomplete", `{"status":"credential_store"}`, "", "could not be read"},
		{"rejected", `{"status":"bad_credentials"}`, "", "rejected"},
		{"challenge", `{"status":"verification_required"}`, "", "human verification"},
		{"empty", `{"status":"ok","access_token":""}`, "", "failed"},
		{"header injection", `{"status":"ok","access_token":"fixture\r\nSecret: fixture-secret"}`, "", "invalid access token"},
		{"unknown status", `{"status":"fixture-secret"}`, "", "failed"},
		{"malformed", `fixture-secret`, "", "invalid response"},
		{"trailing data", `{"status":"ok","access_token":"fixture-secret"} garbage`, "", "invalid response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeCVAToken([]byte(tc.input))
			if got != tc.want {
				t.Fatal("unexpected token result")
			}
			if tc.message == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.message) || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatalf("unexpected or unsafe error: %v", err)
			}
		})
	}
}

func TestFindCVARoot(t *testing.T) {
	root := t.TempDir()
	packagePath := filepath.Join(root, "crunchyroll_video_analyzer")
	if err := os.Mkdir(packagePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packagePath, "credential_store.py"), nil, 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CRUNCHYROLL_CVA_ROOT", root)
	got, err := findCVARoot("")
	if err != nil || got != root {
		t.Fatalf("environment root not resolved: %v", err)
	}
	if _, err := findCVARoot(filepath.Join(root, "missing")); err == nil {
		t.Fatal("an explicit invalid root must not fall back to the environment")
	}
}

func TestCookieProviderDoesNotRequireCVA(t *testing.T) {
	useTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("etp_rt")
		if err != nil || cookie.Value != "fixture-cookie" {
			t.Error("cookie override was lost")
		}
		io.WriteString(w, `{"access_token":"fixture-token"}`)
	})
	provider, err := newAccessTokenProvider("fixture-cookie", cvaAuthOptions{Root: "missing", Python: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := provider()
	if err != nil || got != "fixture-token" {
		t.Fatal("cookie authentication failed")
	}
}

func TestDoRequestUsesSelectedProvider(t *testing.T) {
	oldProvider, oldToken := accessTokenProvider, token
	t.Cleanup(func() { accessTokenProvider, token = oldProvider, oldToken })
	refreshes := 0
	accessTokenProvider = func() (string, error) { refreshes++; return "fixture-fresh", nil }
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fixture-fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.Copy(w, r.Body)
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("payload"))
	resp, err := DoRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if refreshes != 1 || calls != 2 || string(body) != "payload" {
		t.Fatal("provider refresh did not retry exactly once with the original body")
	}
	accessTokenProvider = func() (string, error) { return "", errors.New("fixture provider error") }
	req, _ = http.NewRequest(http.MethodGet, server.URL, nil)
	if _, err := DoRequest(req); err == nil || err.Error() != "fixture provider error" {
		t.Fatal("provider failure was not propagated")
	}
}

func TestCVAHelperProtocol(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("Python is required for the bridge process test")
	}
	oldScript := cvaAuthScript
	t.Cleanup(func() { cvaAuthScript = oldScript })
	options := cvaAuthOptions{Python: python, Root: t.TempDir(), SourceURL: "https://www.crunchyroll.com/series/FIXTURE"}
	for _, tc := range []struct {
		name, script, want string
		timeout            time.Duration
	}{
		{"success", `import json,sys; assert json.load(sys.stdin)["source_url"].startswith("https://"); print('{"status":"ok","access_token":"fixture-token"}')`, "fixture-token", 5 * time.Second},
		{"stderr", `import sys; print("fixture-secret", file=sys.stderr); raise RuntimeError("fixture-secret")`, "", 5 * time.Second},
		{"oversized", `print("fixture-secret" * 100000)`, "", 5 * time.Second},
		{"timeout", `import time; time.sleep(30)`, "", 250 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cvaAuthScript = tc.script
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			got, err := getCVAToken(ctx, options)
			if got != tc.want || (err == nil) != (tc.want != "") {
				t.Fatalf("unexpected process result: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("child diagnostics leaked")
			}
		})
	}
}

func TestInputURLs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urls.txt")
	if err := os.WriteFile(path, []byte("# comment\n\n https://www.crunchyroll.com/series/FIXTURE \n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := inputURLs("ignored", path)
	if err != nil || len(got) != 1 || strings.Contains(got[0], " ") {
		t.Fatalf("invalid batch input: %v", err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inputURLs("", path); err == nil {
		t.Fatal("empty batch accepted")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 70*1024)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inputURLs("", path); err == nil {
		t.Fatal("scanner failure ignored")
	}
}
