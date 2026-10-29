package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Session secrets are exchanged through private pipes and CVA secure stores,
// never process arguments, environment variables, logs, or progress events.
type storedSession struct {
	Cookie    string `json:"cookie"`
	DeviceID  string `json:"device_id"`
	ExpiresAt int64  `json:"expires_at"` // zero means no reliable expiry was supplied
}

func validCookieValue(value string) bool {
	if len(value) == 0 || len(value) > 16384 {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e || char == '"' || char == ';' || char == ',' || char == '\\' {
			return false
		}
	}
	return true
}

func (s storedSession) valid() bool {
	return validCookieValue(s.Cookie) && validCookieValue(s.DeviceID) && s.ExpiresAt >= 0 && s.ExpiresAt <= 253402300799
}

type sessionTokenProvider struct {
	mu sync.Mutex
	// A manually supplied cookie takes precedence, and is saved only after it
	// authenticates. Later token refreshes then use the same stored-cookie path.
	explicit *storedSession
	read     func() (*storedSession, error)
	save     func(storedSession) error
	refresh  func(force bool) (*storedSession, error)
	exchange func(storedSession) (string, storedSession, error)
	now      func() time.Time
}

func (p *sessionTokenProvider) token() (token string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer func() { err = sessionAuthenticationError(err) }()
	manual := p.explicit != nil
	session := p.explicit
	if session == nil {
		reportAuthStep("checking_session")
		var err error
		session, err = p.read()
		if err != nil {
			return "", err
		}
	}
	if session != nil && !session.valid() {
		return "", cvaStatusError("session_invalid")
	}
	if session != nil && (manual || session.ExpiresAt == 0 || session.ExpiresAt > p.now().Unix()) {
		reportAuthStep("validating_session")
		token, updated, err := p.exchange(*session)
		if err == nil {
			if manual || updated != *session {
				reportAuthStep("saving_session")
				if err := p.save(updated); err != nil {
					return "", err
				}
			}
			p.explicit = nil
			reportAuthStep("authenticated")
			return token, nil
		}
		// Invalid manual input must not silently log into a different account.
		// Outages, challenges, malformed responses and rate limits do not prove
		// a cookie expired, so they must not trigger login or overwrite storage.
		if manual || !errors.Is(err, errAuthRejected) {
			return "", err
		}
	}
	reportAuthStep("refreshing_session")
	fresh, err := p.refresh(session != nil)
	if err != nil {
		return "", err
	}
	if fresh == nil || !fresh.valid() {
		return "", cvaStatusError("session_invalid")
	}
	// Confirm the captured browser cookie also works for the downloader before
	// replacing the saved session. There is at most one browser renewal per call.
	reportAuthStep("validating_session")
	token, updated, err := p.exchange(*fresh)
	if err != nil {
		return "", err
	}
	reportAuthStep("saving_session")
	if err := p.save(updated); err != nil {
		return "", err
	}
	reportAuthStep("authenticated")
	return token, nil
}

func newSessionTokenProvider(options cvaAuthOptions, explicit *storedSession) *sessionTokenProvider {
	return &sessionTokenProvider{
		explicit: explicit,
		now:      time.Now,
		exchange: exchangeSessionCookie,
		read:     func() (*storedSession, error) { return readCVASession(options) },
		save:     func(session storedSession) error { return saveCVASession(options, session) },
		refresh:  func(force bool) (*storedSession, error) { return refreshCVASession(options, force) },
	}
}

func decodeCVASession(data []byte) (*storedSession, error) {
	var result struct {
		Status  string         `json:"status"`
		Session *storedSession `json:"session"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, cvaStatusError("session_invalid")
	}
	if result.Status == "session_missing" {
		return nil, nil
	}
	if result.Status != "session_ok" {
		return nil, cvaStatusError(result.Status)
	}
	if result.Session == nil || !result.Session.valid() {
		return nil, cvaStatusError("session_invalid")
	}
	return result.Session, nil
}

func readCVASession(options cvaAuthOptions) (*storedSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	data, err := runCVAHelper(ctx, options, "session-read")
	if err != nil {
		return nil, failure("credential_storage_failed", "authenticating", err)
	}
	return decodeCVASession(data)
}

func saveCVASession(options cvaAuthOptions, session storedSession) error {
	if !session.valid() {
		return cvaStatusError("session_invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	data, err := runCVAHelperRequest(ctx, options, map[string]any{"operation": "session-write", "session": session})
	if err != nil {
		return failure("credential_storage_failed", "authenticating", err)
	}
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(data, &result) != nil {
		return cvaStatusError("session_invalid")
	}
	if result.Status != "session_saved" {
		return cvaStatusError(result.Status)
	}
	return nil
}

func refreshCVASession(options cvaAuthOptions, force bool) (*storedSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	data, err := runCVAHelperRequest(ctx, options, map[string]any{"operation": "session-refresh", "force_refresh": force})
	if err != nil {
		return nil, err
	}
	return decodeCVASession(data)
}
