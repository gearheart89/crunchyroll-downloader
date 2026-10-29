package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

var deviceId = uuid.NewString()

// tokenURL is a variable so tests can point it at a local server.
var tokenURL = "https://www.crunchyroll.com/auth/v1/token"

// errAuthRejected marks a token request Crunchyroll refused, which almost
// always means the etp_rt cookie is wrong or expired.
var errAuthRejected = errors.New("crunchyroll rejected the etp_rt cookie")

type CrunchyrollTokenResponse struct {
	AccessToken string `json:"access_token"`
}

// GetAccessToken fetches an access token from Crunchyroll.
//
// A bad cookie gets an HTTP 400 "invalid_grant" whose JSON still parses, so
// the status and the token itself are checked; otherwise an empty token is
// returned and every later request 401s.
func GetAccessToken(etpRt string) (string, error) {
	reportAuthStep("validating_session")
	token, _, err := exchangeSessionCookie(storedSession{Cookie: etpRt, DeviceID: deviceId})
	if err == nil {
		reportAuthStep("authenticated")
	}
	return token, sessionAuthenticationError(err)
}

// Keep cookies and device identity together, including any rotation supplied
// by the service. Only explicit authentication rejection authorizes re-login.
func exchangeSessionCookie(session storedSession) (string, storedSession, error) {
	if !session.valid() {
		return "", session, errors.New("invalid session cookie or device identity")
	}
	body := url.Values{}
	body.Set("device_id", session.DeviceID)
	body.Set("device_type", "Firefox on Linux")
	body.Set("grant_type", "etp_rt_cookie")

	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", session, err
	}
	req.Header.Set("Authorization", "Basic bm9haWhkZXZtXzZpeWcwYThsMHE6")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")
	req.AddCookie(&http.Cookie{Name: "device_id", Value: session.DeviceID})
	req.AddCookie(&http.Cookie{Name: "etp_rt", Value: session.Cookie})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", session, fmt.Errorf("failed to get access token: %w", err)
	}
	defer resp.Body.Close()

	res, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", session, fmt.Errorf("failed to get access token: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var oauth struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(res, &oauth)
		if resp.StatusCode == http.StatusUnauthorized ||
			((resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden) && oauth.Error == "invalid_grant") {
			return "", session, fmt.Errorf("%w (HTTP %d)", errAuthRejected, resp.StatusCode)
		}
		return "", session, fmt.Errorf("failed to get access token: HTTP %d", resp.StatusCode)
	}

	var result CrunchyrollTokenResponse
	if err := json.Unmarshal(res, &result); err != nil {
		return "", session, errors.New("failed to get access token: invalid response")
	}
	if result.AccessToken == "" {
		return "", session, errors.New("failed to get access token: the response had no access_token")
	}
	for _, char := range result.AccessToken {
		if char <= ' ' || char >= 127 {
			return "", session, errors.New("failed to get access token: invalid bearer")
		}
	}
	updated := session
	for _, cookie := range resp.Cookies() {
		// Cookie deletion after a successful exchange expires the cached
		// session for the next call; it must not become an empty stored secret.
		if cookie.Name == "etp_rt" {
			if cookie.MaxAge < 0 {
				updated.ExpiresAt = time.Now().Unix()
				continue
			}
			if !validCookieValue(cookie.Value) {
				return "", session, errors.New("invalid rotated session cookie")
			}
			updated.Cookie = cookie.Value
			updated.ExpiresAt = 0
			if !cookie.Expires.IsZero() {
				updated.ExpiresAt = max(int64(1), cookie.Expires.Unix())
			}
			if cookie.MaxAge > 0 {
				updated.ExpiresAt = time.Now().Unix() + int64(cookie.MaxAge)
			}
		}
		if cookie.Name == "device_id" && cookie.MaxAge >= 0 && validCookieValue(cookie.Value) {
			updated.DeviceID = cookie.Value
		}
	}
	if !updated.valid() {
		return "", session, errors.New("invalid rotated session metadata")
	}

	return result.AccessToken, updated, nil
}
