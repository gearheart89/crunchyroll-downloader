package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
)

var tokenMu sync.RWMutex
var tokenRefreshMu sync.Mutex

func currentAccessToken() string {
	tokenMu.RLock()
	defer tokenMu.RUnlock()
	return token
}

func refreshAccessToken(previous string) (string, error) {
	tokenRefreshMu.Lock()
	defer tokenRefreshMu.Unlock()
	// Another audio/video worker may already have refreshed this bearer.
	if latest := currentAccessToken(); previous != "" && latest != "" && latest != previous {
		return latest, nil
	}
	newToken, err := accessTokenProvider()
	if err != nil {
		return "", err
	}
	tokenMu.Lock()
	token = newToken
	tokenMu.Unlock()
	events.resumeAfterAuthentication()
	return newToken, nil
}

// DoRequest sends req and, if Crunchyroll answers 401, refreshes the access
// token and retries once. A second 401 is returned as an error rather than
// refreshing again, since it means the new token is being refused too.
func DoRequest(req *http.Request) (*http.Response, error) {
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, failure("network_failed", "resolving", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return checkedResponse(resp)
	}

	print("Access token expired. Refetching one...\n")
	resp.Body.Close()
	previous := ""
	if authorization := req.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
		previous = strings.TrimPrefix(authorization, "Bearer ")
	}
	newToken, err := refreshAccessToken(previous)
	if err != nil {
		return nil, operationFailure(err, "authentication_failed", "authenticating")
	}
	req.Header.Set("Authorization", "Bearer "+newToken)
	// The first attempt already consumed req.Body, so reset it from
	// GetBody before retrying or the retry sends an empty/truncated
	// payload (which broke license requests as "invalid license type").
	if req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			req.Body = body
		}
	}

	resp, err = client.Do(req)
	if err != nil {
		return nil, failure("network_failed", "resolving", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, failure("authentication_failed", "authenticating", fmt.Errorf("%s %s: still unauthorized after refreshing the access token", req.Method, req.URL))
	}
	return checkedResponse(resp)
}

func checkedResponse(resp *http.Response) (*http.Response, error) {
	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		return nil, ErrRateLimited
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, failure("response_failed", "resolving", fmt.Errorf("unexpected HTTP status %d", resp.StatusCode))
	}
	return resp, nil
}
