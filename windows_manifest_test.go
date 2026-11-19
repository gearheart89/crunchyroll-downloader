package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

func TestWindowTemplateTimingAndPinnedRepresentation(t *testing.T) {
	req := testWindowRequest(t)
	manifest := []byte(`<MPD type="static" mediaPresentationDuration="PT6S"><BaseURL>https://cdn.invalid/root/</BaseURL><Period start="PT0S"><AdaptationSet contentType="video"><SegmentTemplate timescale="10" presentationTimeOffset="50" startNumber="7" initialization="$RepresentationID$/init.mp4" media="$RepresentationID$/$Number%03d$-$Time$.m4s"><SegmentTimeline><S t="50" d="20" r="-1"/></SegmentTimeline></SegmentTemplate><Representation id="video/low" height="720"/><Representation id="video/high" height="1080"/></AdaptationSet></Period></MPD>`)
	var calls []string
	fetch := func(ctx context.Context, u, r string, limit int64) ([]byte, error) {
		calls = append(calls, u)
		return []byte("init"), nil
	}
	spans, d, e := parseWindowManifest(context.Background(), manifest, "https://cdn.invalid/manifest.mpd", req, fetch)
	if e != nil {
		t.Fatal(e)
	}
	if rs(d) != "6/1" || len(spans) != 1 || len(spans[0].Segments) != 3 || spans[0].Segments[0].URL != "https://cdn.invalid/root/video/high/007-50.m4s" || rs(spans[0].Segments[0].Start) != "0/1" {
		t.Fatalf("incorrect timeline: %#v", spans)
	}
	if len(calls) != 1 {
		t.Fatal("unexpected acquisition during planning")
	}
	req.Video.Selection = "representation_id"
	id := windowRepID("video/low")
	req.Video.ID = &id
	spans, _, e = parseWindowManifest(context.Background(), manifest, "https://cdn.invalid/manifest.mpd", req, fetch)
	if e != nil || spans[0].RawID != "video/low" {
		t.Fatal("pinned representation not honored", e)
	}
	id = "missing"
	if _, _, e = parseWindowManifest(context.Background(), manifest, "https://cdn.invalid/manifest.mpd", req, fetch); windowErrorCode(e) != "representation_unavailable" {
		t.Fatal("silently substituted unavailable representation", e)
	}
}
func TestWindowOnDemandIndexOffsets(t *testing.T) {
	f := windowFixture(t, false)
	span := f.spans[0]
	sidx := &mp4.SidxBox{Timescale: 24, EarliestPresentationTime: 0, FirstOffset: 13}
	for _, s := range span.Segments {
		sidx.SidxRefs = append(sidx.SidxRefs, mp4.SidxRef{ReferencedSize: uint32(len(f.assets[s.URL])), SubSegmentDuration: 24, StartsWithSAP: 1, SAPType: 1})
	}
	var index bytes.Buffer
	if e := sidx.Encode(&index); e != nil {
		t.Fatal(e)
	}
	initLength := len(span.Init)
	indexStart := initLength
	media := append(append(append([]byte{}, span.Init...), index.Bytes()...), make([]byte, 13)...)
	for _, s := range span.Segments {
		media = append(media, f.assets[s.URL]...)
	}
	manifest := []byte(fmt.Sprintf(`<MPD type="static" mediaPresentationDuration="PT12S"><Period><AdaptationSet contentType="video"><Representation id="v" height="1080"><BaseURL>video.mp4?signature=first</BaseURL><SegmentBase indexRange="%d-%d"><Initialization range="0-%d"/></SegmentBase></Representation></AdaptationSet></Period></MPD>`, indexStart, indexStart+index.Len()-1, initLength-1))
	var ranges []string
	fetch := func(ctx context.Context, u, r string, limit int64) ([]byte, error) {
		ranges = append(ranges, r)
		a, b, e := parseByteRange(r)
		if e != nil || a < 0 || b >= int64(len(media)) {
			t.Fatalf("invalid requested range %s", r)
		}
		return media[a : b+1], nil
	}
	req := testWindowRequest(t)
	spans, d, e := parseWindowManifest(context.Background(), manifest, "https://cdn.invalid/episode.mpd", req, fetch)
	if e != nil {
		t.Fatal(e)
	}
	a, b, e := parseByteRange(spans[0].Segments[0].Range)
	if e != nil || a != int64(initLength+index.Len()+13) || !bytes.Equal(media[a:b+1], f.assets[span.Segments[0].URL]) {
		t.Fatal("incorrect SIDX first_offset")
	}
	if rs(d) != "12/1" || len(ranges) != 2 {
		t.Fatal("index planning downloaded media")
	}
	before := windowSnapshot(req, spans)
	renewed := bytes.ReplaceAll(manifest, []byte("signature=first"), []byte("signature=renewed"))
	updated, _, e := parseWindowManifest(context.Background(), renewed, "https://cdn.invalid/episode.mpd", req, fetch)
	if e != nil || before != windowSnapshot(req, updated) {
		t.Fatal("signed URL changed source identity", e)
	}
	selected, e := loadWindowSamples(context.Background(), spans[0], []int{0, 1, 2}, nil, fetch)
	if e != nil || len(selected.Samples) != 72 {
		t.Fatal("indexed segments not independently readable", e)
	}
}
func TestWindowDiscontinuitiesRequireExplicitSplit(t *testing.T) {
	req := testWindowRequest(t)
	req.Windows = req.Windows[:1]
	req.Windows[0].End = "4/1"
	req.Delivery.Split = false
	manifest := []byte(`<MPD type="static" mediaPresentationDuration="PT4S"><Period start="PT0S" duration="PT2S"><AdaptationSet contentType="video"><SegmentTemplate timescale="1" duration="1" initialization="init" media="$Number$"/><Representation id="v" height="1080"/></AdaptationSet></Period><Period start="PT2S" duration="PT2S"><AdaptationSet contentType="video"><SegmentTemplate timescale="1" duration="1" initialization="init" media="$Number$"/><Representation id="v" height="1080"/></AdaptationSet></Period></MPD>`)
	spans, d, e := parseWindowManifest(context.Background(), manifest, "https://cdn.invalid/a.mpd", req, func(context.Context, string, string, int64) ([]byte, error) { return []byte("init"), nil })
	if e != nil {
		t.Fatal(e)
	}
	result := newWindowResult(req, "attempt")
	w := resolveWindowRequests(req, d, spans, result)
	if len(w) != 0 || !strings.Contains(string(windowJSON(result)), "discontinuity_requires_split") {
		t.Fatal("split policy ignored")
	}
	req.Delivery.Split = true
	result = newWindowResult(req, "attempt")
	w = resolveWindowRequests(req, d, spans, result)
	if len(planWindowGroups(req, w, spans)) != 2 {
		t.Fatal("periods merged")
	}
}

func TestWindowHTTPRangeMustBeHonored(t *testing.T) {
	for _, mode := range []string{"correct", "ignored", "wrong-start", "truncated", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=5-8" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("incorrect request headers")
				}
				if r.UserAgent() != windowUserAgent || r.Header.Get("Origin") != "https://static.crunchyroll.com" || r.Referer() != "https://static.crunchyroll.com/" {
					t.Error("missing media client headers")
				}
				if mode == "ignored" {
					w.Write([]byte("entire file"))
					return
				}
				value := "bytes 5-8/20"
				if mode == "wrong-start" {
					value = "bytes 0-3/20"
				}
				if mode == "malformed" {
					value += "junk"
				}
				w.Header().Set("Content-Range", value)
				w.WriteHeader(206)
				if mode == "truncated" {
					w.Write([]byte("ab"))
				} else {
					w.Write([]byte("abcd"))
				}
			}))
			defer server.Close()
			b, e := fetchWindowBytesUsing(context.Background(), server.URL, "5-8", 100, server.Client())
			if mode == "correct" {
				if e != nil || string(b) != "abcd" {
					t.Fatal(e)
				}
			} else if e == nil {
				t.Fatal("invalid partial response accepted")
			}
		})
	}
}

type windowHTTPRoundTrip func(*http.Request) (*http.Response, error)

func (f windowHTTPRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWindowManifestAuthenticationBoundaries(t *testing.T) {
	oldToken := token
	token = "fixture-bearer"
	t.Cleanup(func() { token = oldToken })
	for _, target := range []string{"https://www.crunchyroll.com/manifest", "https://cdn.invalid/manifest", "https://cdn.www.crunchyroll.com/manifest", "https://www.crunchyroll.com.invalid/manifest", "https://www.crunchyroll.com:444/manifest", "http://www.crunchyroll.com/manifest", "https://user@www.crunchyroll.com/manifest"} {
		for _, redirect := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/redirect=%t", target, redirect), func(t *testing.T) {
				calls := 0
				client := windowMediaClient()
				client.Transport = windowHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					want := ""
					if r.URL.String() == "https://www.crunchyroll.com/manifest" || (redirect && calls == 1) {
						want = "Bearer fixture-bearer"
					}
					if r.Header.Get("Authorization") != want || r.Header.Get("Cookie") != "" || r.UserAgent() != windowUserAgent {
						t.Error("incorrect authentication or client headers")
					}
					resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("manifest")), Request: r}
					if redirect && calls == 1 {
						resp.StatusCode = 302
						resp.Header.Set("Location", target)
					}
					return resp, nil
				})
				locator := target
				if redirect {
					locator = "https://www.crunchyroll.com/initial"
				}
				b, e := fetchWindowManifestUsing(context.Background(), locator, client)
				unsafe := strings.HasPrefix(target, "http:") || strings.Contains(target, "user@")
				if unsafe {
					if e == nil || calls > 1 || (!redirect && calls != 0) {
						t.Fatal("unsafe manifest destination accepted")
					}
				} else if e != nil || string(b) != "manifest" {
					t.Fatal("valid manifest failed", e)
				}
			})
		}
	}
}

func TestWindowManifestRefreshOnlyProviderAndOnlyOnce(t *testing.T) {
	oldToken, oldProvider := token, accessTokenProvider
	t.Cleanup(func() { token, accessTokenProvider = oldToken, oldProvider })
	for _, mode := range []string{"renewed", "rejected", "cdn-rejected"} {
		t.Run(mode, func(t *testing.T) {
			token = "fixture-old"
			calls, refreshes := 0, 0
			accessTokenProvider = func() (string, error) { refreshes++; return "fixture-new", nil }
			client := windowMediaClient()
			client.Transport = windowHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				status := 401
				if mode == "renewed" && r.Header.Get("Authorization") == "Bearer fixture-new" {
					status = 200
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("manifest")), Request: r}, nil
			})
			locator := "https://www.crunchyroll.com/manifest"
			if mode == "cdn-rejected" {
				locator = "https://cdn.invalid/manifest"
			}
			_, e := fetchWindowManifestUsing(context.Background(), locator, client)
			if mode == "cdn-rejected" {
				if calls != 1 || refreshes != 0 || windowErrorCode(e) != "source_unavailable" {
					t.Fatal("CDN rejection renewed account authentication", e)
				}
			} else if calls != 2 || refreshes != 1 || (mode == "renewed" && e != nil) || (mode == "rejected" && windowErrorCode(e) != "authentication_failed") {
				t.Fatal("incorrect bounded renewal", e)
			}
		})
	}
}
