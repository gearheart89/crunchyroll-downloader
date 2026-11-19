package main

import (
	"context"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/iyear/gowidevine"
)

type wmResolved struct {
	Spec             windowSpec
	Content, Context wmInterval
	Result           map[string]any
	Covered          []wmInterval
}
type wmGroup struct {
	Span    int
	Want    wmInterval
	Windows []*wmResolved
}

func resolveWindowRequests(req windowRequest, duration *big.Rat, spans []wmSpan, result map[string]any) []*wmResolved {
	windows := []*wmResolved{}
	for _, w := range req.Windows {
		row := map[string]any{"window_id": w.ID, "status": "failed", "resolved_requested_seconds": nil, "resolved_context_seconds": nil, "clip_ids": []string{}}
		result["windows"] = append(result["windows"].([]any), row)
		a, b := rat(w.Start), rat(w.End)
		if w.Anchor == "episode_end" {
			a = add(a, duration)
			b = add(b, duration)
		}
		if b.Sign() <= 0 || a.Cmp(duration) >= 0 || (w.Policy == "require_requested_interval" && (a.Sign() < 0 || b.Cmp(duration) > 0)) {
			wmAddError(result, w.ID, "coverage_incomplete")
			continue
		}
		a = maxRat(a, new(big.Rat))
		b = minRat(b, duration)
		content := wmInterval{a, b}
		context := wmInterval{maxRat(sub(a, rat(w.Before)), new(big.Rat)), minRat(add(b, rat(w.After)), duration)}
		row["resolved_requested_seconds"] = []string{rs(a), rs(b)}
		row["resolved_context_seconds"] = []string{rs(context.Start), rs(context.End)}
		count := 0
		for _, span := range spans {
			if wmIntersects(context, wmInterval{span.Start, span.End}) {
				count++
			}
		}
		if count > 1 && !req.Delivery.Split {
			wmAddError(result, w.ID, "discontinuity_requires_split")
			continue
		}
		windows = append(windows, &wmResolved{w, content, context, row, nil})
	}
	return windows
}
func wmAddError(result map[string]any, id any, code string) {
	for _, existing := range result["errors"].([]any) {
		if existing.(map[string]any)["window_id"] == id {
			return
		}
	}
	result["errors"] = append(result["errors"].([]any), map[string]any{"code": code, "window_id": id, "message": "Requested window coverage could not be validated."})
}
func planWindowGroups(req windowRequest, windows []*wmResolved, spans []wmSpan) []wmGroup {
	groups := []wmGroup{}
	for i, s := range spans {
		candidates := []wmGroup{}
		for _, w := range windows {
			if wmIntersects(w.Context, wmInterval{s.Start, s.End}) {
				candidates = append(candidates, wmGroup{i, wmInterval{maxRat(w.Context.Start, s.Start), minRat(w.Context.End, s.End)}, []*wmResolved{w}})
			}
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Want.Start.Cmp(candidates[j].Want.Start) < 0 })
		for _, g := range candidates {
			if len(groups) > 0 {
				last := &groups[len(groups)-1]
				if last.Span == i && sub(g.Want.Start, rat(req.Delivery.Before)).Cmp(add(last.Want.End, rat(req.Delivery.After))) <= 0 {
					last.Want.End = maxRat(last.Want.End, g.Want.End)
					last.Windows = append(last.Windows, g.Windows...)
					continue
				}
			}
			groups = append(groups, g)
		}
	}
	return groups
}
func windowCoverageComplete(want wmInterval, covered []wmInterval) bool {
	sort.Slice(covered, func(i, j int) bool { return covered[i].Start.Cmp(covered[j].Start) < 0 })
	cursor := want.Start
	for _, part := range covered {
		if part.End.Cmp(cursor) <= 0 {
			continue
		}
		if part.Start.Cmp(cursor) > 0 {
			return false
		}
		cursor = part.End
		if cursor.Cmp(want.End) >= 0 {
			return true
		}
	}
	return false
}

func acquireMediaWindows(ctx context.Context, req windowRequest, stage string, reporter *windowReporter, result map[string]any) error {
	if e := wmMediaToolsAvailable(); e != nil {
		return e
	}
	reporter.phase("resolving")
	var metadata EpisodeMetadataResponse
	if e := windowAPI(ctx, "https://www.crunchyroll.com/content/v2/cms/objects/"+url.PathEscape(req.Source.WatchID)+"?ratings=true&locale=en-US", &metadata); e != nil {
		return e
	}
	if len(metadata.Data) != 1 {
		return wf("source_unavailable")
	}
	versions := buildGuidByLocale(metadata.Data[0], req.Source.WatchID)
	found := false
	locale := ""
	for lang, id := range versions {
		if id == req.Source.PresentationID && (req.Source.Locale == nil || lang == *req.Source.Locale) {
			found = true
			locale = lang
			break
		}
	}
	if !found {
		return wf("source_unavailable")
	}
	var episode Episode
	if e := windowAPI(ctx, "https://www.crunchyroll.com/playback/v3/"+url.PathEscape(req.Source.PresentationID)+"/web/firefox/play", &episode); e != nil {
		return e
	}
	if episode.Error != "" || episode.ManifestURL == "" {
		return wf("source_unavailable")
	}
	defer func() {
		if episode.Token != "" {
			if releaseWindowPlayback(req.Source.PresentationID, episode.Token) != nil {
				reporter.emit(map[string]any{"event": "warning", "code": "nonfatal_network_warning", "message": "The playback session could not be released."})
			}
		}
	}()
	manifest, e := fetchWindowManifest(ctx, episode.ManifestURL)
	if e != nil {
		return e
	}
	spans, duration, e := parseWindowManifest(ctx, manifest, episode.ManifestURL, req, fetchWindowBytes)
	if e != nil {
		return e
	}
	snapshot := windowSnapshot(req, spans)
	if req.Source.Snapshot != nil && *req.Source.Snapshot != snapshot {
		return wf("source_changed")
	}
	result["source"] = map[string]any{"provider": "crunchyroll", "watch_id": req.Source.WatchID, "presentation_id": req.Source.PresentationID, "presentation_locale": locale, "provider_revision": nil, "snapshot_id": snapshot, "episode_duration_seconds": rs(duration)}
	keys := map[int][]*widevine.Key{}
	keyProvider := func(i int) ([]*widevine.Key, error) {
		if k, ok := keys[i]; ok {
			return k, nil
		}
		init, e := inspectWindowInit(spans[i].Init)
		if e != nil {
			return nil, e
		}
		if init.Entry.Sinf == nil {
			keys[i] = nil
			return nil, nil
		}
		if spans[i].PSSH == "" {
			return nil, wf("source_unavailable")
		}
		k, e := getLicenseContext(ctx, spans[i].PSSH, req.Source.PresentationID, episode.Token)
		if e != nil {
			return nil, wf("source_unavailable")
		}
		keys[i] = k
		return k, nil
	}
	return acquireWindowSpans(ctx, req, spans, duration, stage, reporter, result, fetchWindowBytes, keyProvider)
}

// The injectable byte reader and key provider permit end-to-end acquisition
// acceptance tests without account credentials, a browser or external network.
func acquireWindowSpans(ctx context.Context, req windowRequest, spans []wmSpan, duration *big.Rat, stage string, reporter *windowReporter, result map[string]any, fetch wmFetch, keyProvider func(int) ([]*widevine.Key, error)) error {
	windows := resolveWindowRequests(req, duration, spans, result)
	groups := planWindowGroups(req, windows, spans)
	if len(groups) > 128 {
		return wf("unsupported_capability")
	}
	totals, completed := map[string]int{}, map[string]int{}
	for _, g := range groups {
		n := len(windowGroupIndexes(req, g, spans[g.Span]))
		for _, w := range g.Windows {
			totals[w.Spec.ID] += n
		}
	}
	cacheDir := filepath.Join(stage, "work")
	if e := os.MkdirAll(cacheDir, 0700); e != nil {
		return wf("output_configuration_failed")
	}
	cached := map[string]string{}
	defer finishResolvedWindows(windows, result)
	for _, g := range groups {
		if ctx.Err() != nil {
			return wf("cancelled")
		}
		if reporter.err != nil {
			return wf("output_configuration_failed")
		}
		span := spans[g.Span]
		indexes := windowGroupIndexes(req, g, span)
		if len(indexes) == 0 {
			for _, w := range g.Windows {
				wmAddError(result, w.Spec.ID, "coverage_incomplete")
			}
			continue
		}
		var properties map[string]any
		var selected []wmSample
		var video *wmVideo
		var clip map[string]any
		clipID := "clip-" + windowHash(windowJSON([]any{span.ID, rs(g.Want.Start), rs(g.Want.End)}))[:20]
		path := filepath.Join(stage, clipID+".mp4")
		err := func() error {
			keys, e := keyProvider(g.Span)
			if e != nil {
				return e
			}
			reporter.phase("downloading")
			groupFetch := func(ctx context.Context, u, byteRange string, limit int64) ([]byte, error) {
				key := windowHash(windowJSON([]string{u, byteRange}))
				var b []byte
				var e error
				if path, ok := cached[key]; ok {
					b, e = os.ReadFile(path)
				} else {
					b, e = fetch(ctx, u, byteRange, limit)
					if e == nil {
						path := filepath.Join(cacheDir, key)
						e = os.WriteFile(path, b, 0600)
						if e == nil {
							cached[key] = path
						}
					}
				}
				if e != nil {
					return nil, e
				}
				for _, w := range g.Windows {
					id := w.Spec.ID
					completed[id]++
					reporter.emit(map[string]any{"event": "window_progress", "window_id": id, "unit": "segments", "units_completed": completed[id], "units_total": totals[id], "percent": 100 * completed[id] / totals[id]})
				}
				if reporter.err != nil {
					return nil, wf("output_configuration_failed")
				}
				return b, nil
			}
			video, e = loadWindowSamples(ctx, span, indexes, keys, groupFetch)
			if e != nil {
				return e
			}
			selected, e = selectWindowSamples(video, span, g.Want, rat(req.Delivery.Before), rat(req.Delivery.After), indexes[len(indexes)-1] == len(span.Segments)-1)
			if e != nil {
				return e
			}
			reporter.phase("assembling")
			requested, contexts := []wmInterval{}, []wmInterval{}
			for _, w := range windows {
				requested = append(requested, w.Content)
				contexts = append(contexts, w.Context)
			}
			properties, e = writeWindowClip(ctx, video, selected, path, clipID, requested, contexts)
			if e != nil {
				return e
			}
			properties["id"] = span.RepresentationID
			if existing, ok := result["representation"].(map[string]any); ok && !reflect.DeepEqual(existing, properties) {
				if existing["codec_configuration_sha256"] != properties["codec_configuration_sha256"] {
					return wf("codec_configuration_changed")
				}
				return wf("representation_changed")
			}
			reporter.phase("validating")
			clip, e = wmClipResult(path, clipID, span.ID, selected, video.Scale)
			return e
		}()
		if err != nil {
			os.Remove(path)
			os.Remove(strings.TrimSuffix(path, ".mp4") + ".timeline.jsonl")
			for _, w := range g.Windows {
				wmAddError(result, w.Spec.ID, windowErrorCode(err))
			}
			continue
		}
		result["representation"] = properties
		result["clips"] = append(result["clips"].([]any), clip)
		for _, w := range g.Windows {
			w.Result["clip_ids"] = append(w.Result["clip_ids"].([]string), clipID)
			w.Covered = append(w.Covered, g.Want)
		}
	}
	return nil
}

func finishResolvedWindows(windows []*wmResolved, result map[string]any) {
	for _, w := range windows {
		if windowCoverageComplete(w.Context, w.Covered) {
			w.Result["status"] = "completed"
		} else {
			if len(w.Covered) > 0 {
				w.Result["status"] = "partial"
			}
			hasError := false
			for _, e := range result["errors"].([]any) {
				if e.(map[string]any)["window_id"] == w.Spec.ID {
					hasError = true
				}
			}
			if !hasError {
				wmAddError(result, w.Spec.ID, "coverage_incomplete")
			}
		}
	}
}

func windowGroupIndexes(req windowRequest, g wmGroup, span wmSpan) []int {
	lo := sub(g.Want.Start, rat(req.Delivery.Before))
	hi := add(g.Want.End, rat(req.Delivery.After))
	indexes := []int{}
	for i, seg := range span.Segments {
		if seg.Start.Cmp(hi) <= 0 && seg.End.Cmp(lo) > 0 {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func releaseWindowPlayback(id, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, http.MethodDelete, "https://www.crunchyroll.com/playback/v1/token/"+url.PathEscape(id)+"/"+url.PathEscape(token), nil)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+currentAccessToken())
	req.Header.Set("User-Agent", windowUserAgent)
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		return e
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return wf("network_failed")
	}
	return nil
}
