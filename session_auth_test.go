package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionProviderReuseAndRenewal(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, mode := range []string{"valid", "missing", "expired", "rejected", "network", "rate_limit", "rotation", "manual", "manual_rejected", "locked", "save_error", "fresh_rejected"} {
		t.Run(mode, func(t *testing.T) {
			buf := captureEvents(t)
			events.setPhase("authenticating")
			old := storedSession{Cookie: "fixture-old", DeviceID: "fixture-device", ExpiresAt: now.Unix() + 3600}
			fresh := storedSession{Cookie: "fixture-new", DeviceID: "fixture-device", ExpiresAt: now.Unix() + 7200}
			reads, refreshes, exchanges, saves := 0, 0, 0, 0
			var stored storedSession
			p := &sessionTokenProvider{
				now: func() time.Time { return now },
				read: func() (*storedSession, error) {
					reads++
					if mode == "locked" {
						return nil, cvaStatusError("credential_locked")
					}
					if mode == "missing" || mode == "fresh_rejected" {
						return nil, nil
					}
					copy := old
					if mode == "expired" {
						copy.ExpiresAt = now.Unix() - 1
					}
					return &copy, nil
				},
				save: func(s storedSession) error {
					saves++
					stored = s
					if mode == "save_error" {
						return cvaStatusError("credential_unavailable")
					}
					return nil
				},
				refresh: func(force bool) (*storedSession, error) {
					refreshes++
					if force != (mode != "missing" && mode != "fresh_rejected") {
						t.Error("wrong renewal mode")
					}
					return &fresh, nil
				},
				exchange: func(s storedSession) (string, storedSession, error) {
					exchanges++
					if mode == "network" || mode == "rate_limit" {
						return "", s, errors.New("temporary request failure")
					}
					if (mode == "rejected" && s.Cookie == old.Cookie) || mode == "manual_rejected" || mode == "fresh_rejected" {
						return "", s, errAuthRejected
					}
					if mode == "rotation" || mode == "save_error" {
						return "fixture-token", fresh, nil
					}
					return "fixture-token", s, nil
				},
			}
			if strings.HasPrefix(mode, "manual") {
				p.explicit = &old
			}
			token, err := p.token()
			wantError := mode == "network" || mode == "rate_limit" || mode == "manual_rejected" || mode == "locked" || mode == "save_error" || mode == "fresh_rejected"
			if (err != nil) != wantError || (token != "") == wantError {
				t.Fatalf("unexpected authentication outcome: %v", err)
			}
			wantRefresh := mode == "missing" || mode == "expired" || mode == "rejected" || mode == "fresh_rejected"
			if refreshes != boolInt(wantRefresh) {
				t.Fatalf("browser renewals = %d", refreshes)
			}
			wantSave := !wantError && (wantRefresh || mode == "rotation" || mode == "manual") || mode == "save_error"
			if saves != boolInt(wantSave) {
				t.Fatalf("store writes = %d", saves)
			}
			if mode == "valid" && (reads != 1 || exchanges != 1) {
				t.Fatal("valid cookie was not used directly")
			}
			if mode == "expired" && exchanges != 1 {
				t.Fatal("expired cookie sent before renewal")
			}
			if mode == "rejected" && exchanges != 2 {
				t.Fatal("renewed cookie not validated")
			}
			if mode == "manual" && (reads != 0 || stored != old || p.explicit != nil) {
				t.Fatal("manual cookie was not validated, saved and consumed")
			}
			if wantSave && mode != "manual" && stored != fresh {
				t.Fatal("replacement session lost")
			}
			if err == nil {
				events.startAttempt()
				events.output(filepath.Join(t.TempDir(), "episode.mp4"), false)
			}
			exit := events.finish(err, "mp4")
			records := checkProtocol(t, buf.Bytes(), exit)
			steps := map[string]string{
				"valid":           "checking_session validating_session authenticated",
				"missing":         "checking_session refreshing_session validating_session saving_session authenticated",
				"expired":         "checking_session refreshing_session validating_session saving_session authenticated",
				"rejected":        "checking_session validating_session refreshing_session validating_session saving_session authenticated",
				"network":         "checking_session validating_session",
				"rate_limit":      "checking_session validating_session",
				"rotation":        "checking_session validating_session saving_session authenticated",
				"manual":          "validating_session saving_session authenticated",
				"manual_rejected": "validating_session",
				"locked":          "checking_session",
				"save_error":      "checking_session validating_session saving_session",
				"fresh_rejected":  "checking_session refreshing_session validating_session",
			}
			assertAuthSteps(t, records, steps[mode])
			wantReason := map[string]string{"manual_rejected": "session_rejected", "fresh_rejected": "session_rejected", "locked": "credential_locked", "save_error": "credential_unavailable"}[mode]
			last := records[len(records)-1]
			if reason, _ := last["reason"].(string); reason != wantReason {
				t.Fatalf("reason = %q, want %q", reason, wantReason)
			}
			if strings.Contains(buf.String(), "fixture-") || strings.Contains(buf.String(), "temporary request failure") {
				t.Fatal("session data or raw error leaked into progress")
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestSessionProviderSerializesRenewal(t *testing.T) {
	var stored *storedSession
	refreshes := 0
	p := &sessionTokenProvider{
		now:  time.Now,
		read: func() (*storedSession, error) { return stored, nil },
		save: func(s storedSession) error { stored = &s; return nil },
		refresh: func(bool) (*storedSession, error) {
			refreshes++
			return &storedSession{Cookie: "fixture-cookie", DeviceID: "fixture-device"}, nil
		},
		exchange: func(s storedSession) (string, storedSession, error) { return "fixture-token", s, nil },
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.token(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if refreshes != 1 {
		t.Fatalf("parallel calls renewed %d times", refreshes)
	}
}

func TestSessionCookieExchangeAndRotation(t *testing.T) {
	original := storedSession{Cookie: "fixture-cookie", DeviceID: "persistent-device", ExpiresAt: 1800000000}
	useTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		device, err := r.Cookie("device_id")
		if err != nil || device.Value != original.DeviceID || r.Form.Get("device_id") != original.DeviceID {
			t.Error("device identity was not preserved")
		}
		cookie, err := r.Cookie("etp_rt")
		if err != nil || cookie.Value != original.Cookie {
			t.Error("stored cookie was not sent")
		}
		http.SetCookie(w, &http.Cookie{Name: "etp_rt", Value: "fixture-rotated", MaxAge: 3600, Secure: true})
		http.SetCookie(w, &http.Cookie{Name: "device_id", Value: "rotated-device", Secure: true})
		io.WriteString(w, `{"access_token":"fixture-bearer","expires_in":300}`)
	})
	bearer, updated, err := exchangeSessionCookie(original)
	if err != nil || bearer != "fixture-bearer" || updated.Cookie != "fixture-rotated" || updated.DeviceID != "rotated-device" {
		t.Fatalf("rotation failed: %v", err)
	}
	if remaining := updated.ExpiresAt - time.Now().Unix(); remaining < 3598 || remaining > 3600 {
		t.Fatal("cookie expiry confused with bearer lifetime")
	}
}

func TestOnlyAuthRejectionTriggersSessionRenewal(t *testing.T) {
	for _, tc := range []struct {
		status   int
		body     string
		rejected bool
	}{
		{400, `{"error":"invalid_grant"}`, true}, {401, `{}`, true}, {403, `{"error":"invalid_grant"}`, true},
		{400, `bad request`, false}, {403, `<html>challenge</html>`, false}, {429, `{}`, false}, {500, `{}`, false}, {200, `not-json`, false},
	} {
		useTokenServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) })
		_, _, err := exchangeSessionCookie(storedSession{Cookie: "fixture-cookie", DeviceID: "fixture-device"})
		if err == nil || errors.Is(err, errAuthRejected) != tc.rejected {
			t.Fatalf("HTTP %d classified incorrectly: %v", tc.status, err)
		}
	}
}

func TestStoredSessionProtocolValidation(t *testing.T) {
	for _, input := range []string{
		`{"status":"session_ok","session":{"cookie":"fixture-secret\n","device_id":"id","expires_at":0}}`,
		`{"status":"session_ok","session":{"cookie":"fixture-secret;injected","device_id":"id","expires_at":0}}`,
		`{"status":"session_ok","session":{"cookie":"fixture-secret","device_id":"id","expires_at":-1}}`,
		`{"status":"session_ok"}`, `fixture-secret`, `{"status":"fixture-secret"}`,
	} {
		if _, err := decodeCVASession([]byte(input)); err == nil || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("invalid or unsafe protocol result: %v", err)
		}
	}
	if session, err := decodeCVASession([]byte(`{"status":"session_missing"}`)); err != nil || session != nil {
		t.Fatal("missing cache was not accepted")
	}
}

func TestSessionHelperKeepsCookieInPrivatePipe(t *testing.T) {
	python := testPython(t)
	previous := cvaAuthScript
	t.Cleanup(func() { cvaAuthScript = previous })
	cvaAuthScript = `import json,sys
request=json.load(sys.stdin)
assert all("fixture-secret" not in arg for arg in sys.argv)
assert request["operation"] == "session-write"
assert request["session"]["cookie"] == "fixture-secret"
print('{"status":"session_saved"}')`
	if err := saveCVASession(cvaAuthOptions{Root: t.TempDir(), Python: python}, storedSession{Cookie: "fixture-secret", DeviceID: "fixture-device"}); err != nil {
		t.Fatal(err)
	}
	cvaAuthScript = `import json,sys
request=json.load(sys.stdin)
assert request["operation"] == "session-refresh" and request["force_refresh"] is True
print('{"status":"session_refresh_unsupported"}')`
	if _, err := refreshCVASession(cvaAuthOptions{Root: t.TempDir(), Python: python}, true); err == nil || !strings.Contains(err.Error(), "requires Windows") {
		t.Fatalf("missing Docker renewal diagnostic: %v", err)
	}
	// The same private pipe is bounded and never logs a malformed secret reply.
	cvaAuthScript = `print("fixture-secret")`
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data, err := runCVAHelper(ctx, cvaAuthOptions{Root: t.TempDir(), Python: python}, "session-read")
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeCVASession(data)
	if err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("malformed helper response leaked")
	}
}

func TestSessionProviderAcrossInvocations(t *testing.T) {
	// A fresh provider models another downloader process. Only the encrypted
	// store's serialization persists; no in-memory cookie can satisfy this test.
	var storage []byte
	refreshes := 0
	newProvider := func() *sessionTokenProvider {
		return &sessionTokenProvider{
			now: time.Now,
			read: func() (*storedSession, error) {
				if len(storage) == 0 {
					return nil, nil
				}
				var s storedSession
				err := json.Unmarshal(storage, &s)
				return &s, err
			},
			save: func(s storedSession) error { var err error; storage, err = json.Marshal(s); return err },
			refresh: func(bool) (*storedSession, error) {
				refreshes++
				return &storedSession{Cookie: "fixture-cookie", DeviceID: "fixture-device"}, nil
			},
			exchange: func(s storedSession) (string, storedSession, error) { return "fixture-token", s, nil },
		}
	}
	for range 2 {
		if _, err := newProvider().token(); err != nil {
			t.Fatal(err)
		}
	}
	if refreshes != 1 {
		t.Fatal("new invocation did not reuse stored cookie")
	}
}

func TestCVAProviderUsesStoredCookieWithoutBrowser(t *testing.T) {
	python := testPython(t)
	root := t.TempDir()
	packagePath := filepath.Join(root, "crunchyroll_video_analyzer")
	if err := os.Mkdir(packagePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packagePath, "credential_store.py"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	previous := cvaAuthScript
	t.Cleanup(func() { cvaAuthScript = previous })
	cvaAuthScript = `import json,sys
request=json.load(sys.stdin)
assert request["operation"] == "session-read", "unexpected browser or storage mutation"
print('{"status":"session_ok","session":{"cookie":"fixture-cookie","device_id":"persisted-device","expires_at":0}}')`
	useTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("etp_rt")
		if err != nil || cookie.Value != "fixture-cookie" {
			t.Error("cache not used")
		}
		device, err := r.Cookie("device_id")
		if err != nil || device.Value != "persisted-device" {
			t.Error("stored device not used")
		}
		io.WriteString(w, `{"access_token":"fixture-token"}`)
	})
	for range 2 {
		provider, err := newAccessTokenProvider("", cvaAuthOptions{Root: root, Python: python})
		if err != nil {
			t.Fatal(err)
		}
		if bearer, err := provider(); err != nil || bearer != "fixture-token" {
			t.Fatalf("cache reuse failed: %v", err)
		}
	}
}
