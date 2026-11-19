package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

type wmTimelineEntry struct {
	T *int64 `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R int64  `xml:"r,attr"`
}
type wmTemplate struct {
	Timescale *int64 `xml:"timescale,attr"`
	PTO       *int64 `xml:"presentationTimeOffset,attr"`
	Duration  *int64 `xml:"duration,attr"`
	Start     *int64 `xml:"startNumber,attr"`
	Init      string `xml:"initialization,attr"`
	Media     string `xml:"media,attr"`
	Timeline  *struct {
		Entries []wmTimelineEntry `xml:"S"`
	} `xml:"SegmentTimeline"`
}
type wmSegmentBase struct {
	Timescale *int64 `xml:"timescale,attr"`
	PTO       *int64 `xml:"presentationTimeOffset,attr"`
	Index     string `xml:"indexRange,attr"`
	Init      struct {
		Range  string `xml:"range,attr"`
		Source string `xml:"sourceURL,attr"`
	} `xml:"Initialization"`
}
type wmProtection struct {
	Scheme string `xml:"schemeIdUri,attr"`
	PSSH   string `xml:"pssh"`
}
type wmRepresentation struct {
	ID          string         `xml:"id,attr"`
	Height      int64          `xml:"height,attr"`
	Width       int64          `xml:"width,attr"`
	Bandwidth   int64          `xml:"bandwidth,attr"`
	Mime        string         `xml:"mimeType,attr"`
	Base        string         `xml:"BaseURL"`
	Template    *wmTemplate    `xml:"SegmentTemplate"`
	SegmentBase *wmSegmentBase `xml:"SegmentBase"`
	Protection  []wmProtection `xml:"ContentProtection"`
}
type wmAdaptation struct {
	Type            string             `xml:"contentType,attr"`
	Mime            string             `xml:"mimeType,attr"`
	Base            string             `xml:"BaseURL"`
	Template        *wmTemplate        `xml:"SegmentTemplate"`
	SegmentBase     *wmSegmentBase     `xml:"SegmentBase"`
	Protection      []wmProtection     `xml:"ContentProtection"`
	Representations []wmRepresentation `xml:"Representation"`
}
type wmPeriod struct {
	ID          string         `xml:"id,attr"`
	Start       string         `xml:"start,attr"`
	Duration    string         `xml:"duration,attr"`
	Base        string         `xml:"BaseURL"`
	Template    *wmTemplate    `xml:"SegmentTemplate"`
	SegmentBase *wmSegmentBase `xml:"SegmentBase"`
	Sets        []wmAdaptation `xml:"AdaptationSet"`
}
type wmManifest struct {
	Type     string     `xml:"type,attr"`
	Duration string     `xml:"mediaPresentationDuration,attr"`
	Base     string     `xml:"BaseURL"`
	Periods  []wmPeriod `xml:"Period"`
}
type wmSegment struct {
	URL        string
	Range      string
	Start, End *big.Rat
	Number     int64
}
type wmSpan struct {
	ID, RepresentationID, RawID, PSSH string
	Start, End, PTO                   *big.Rat
	Init                              []byte
	Segments                          []wmSegment
}
type wmFetch func(context.Context, string, string, int64) ([]byte, error)

const windowUserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0"

func windowProviderOrigin(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil && strings.EqualFold(u.Hostname(), "www.crunchyroll.com") && (u.Port() == "" || u.Port() == "443")
}

func windowMediaClient() *http.Client {
	return &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil {
			return wf("network_failed")
		}
		// Go normally forwards auth to subdomains, which is too broad here.
		if !windowProviderOrigin(req.URL) {
			req.Header.Del("Authorization")
		}
		req.Header.Del("Cookie")
		return nil
	}}
}

// Playback can return an authenticated provider manifest, rather than a signed
// CDN locator. Only the exact provider origin receives the access token.
func fetchWindowManifest(ctx context.Context, locator string) ([]byte, error) {
	return fetchWindowManifestUsing(ctx, locator, windowMediaClient())
}

func fetchWindowManifestUsing(ctx context.Context, locator string, client *http.Client) ([]byte, error) {
	u, e := url.Parse(locator)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return nil, wf("source_unavailable")
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, locator, nil)
		if e != nil {
			return nil, wf("source_unavailable")
		}
		req.Header.Set("User-Agent", windowUserAgent)
		req.Header.Set("Accept-Encoding", "identity")
		previous := ""
		if windowProviderOrigin(u) {
			previous = currentAccessToken()
			req.Header.Set("Authorization", "Bearer "+previous)
		}
		resp, e := client.Do(req)
		if e != nil {
			return nil, wf("network_failed")
		}
		if resp.StatusCode == http.StatusUnauthorized && previous != "" && windowProviderOrigin(resp.Request.URL) {
			resp.Body.Close()
			if attempt == 1 {
				return nil, wf("authentication_failed")
			}
			if _, e = refreshAccessToken(previous); e != nil {
				return nil, wf("authentication_failed")
			}
			continue
		}
		b, e := readWindowResponse(resp, "", 16<<20)
		resp.Body.Close()
		return b, e
	}
	return nil, wf("authentication_failed")
}

func fetchWindowBytes(ctx context.Context, locator, byteRange string, limit int64) ([]byte, error) {
	u, e := url.Parse(locator)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return nil, wf("source_unavailable")
	}
	// No bearer/cookie is sent to media CDN hosts; signed locators carry their
	// own authorization. Redirects may not downgrade to HTTP.
	return fetchWindowBytesUsing(ctx, locator, byteRange, limit, windowMediaClient())
}

func fetchWindowBytesUsing(ctx context.Context, locator, byteRange string, limit int64, client *http.Client) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", locator, nil)
	if e != nil {
		return nil, wf("network_failed")
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", windowUserAgent)
	req.Header.Set("Origin", "https://static.crunchyroll.com")
	req.Header.Set("Referer", "https://static.crunchyroll.com/")
	if byteRange != "" {
		req.Header.Set("Range", "bytes="+byteRange)
	}
	resp, e := client.Do(req)
	if e != nil {
		return nil, wf("network_failed")
	}
	defer resp.Body.Close()
	return readWindowResponse(resp, byteRange, limit)
}

func readWindowResponse(resp *http.Response, byteRange string, limit int64) ([]byte, error) {
	if resp.StatusCode == 429 {
		return nil, wf("rate_limited")
	}
	if byteRange != "" {
		a, b, e := parseByteRange(byteRange)
		if e != nil || a < 0 || b < a || b-a >= limit {
			return nil, wf("source_unavailable")
		}
		var start, end, total int64
		if resp.StatusCode != 206 {
			return nil, wf("network_failed")
		}
		if n, _ := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); n != 3 || start != a || end != b || total <= end || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", start, end, total) {
			return nil, wf("network_failed")
		}
		limit = b - a + 1
	} else if resp.StatusCode != 200 {
		return nil, wf("source_unavailable")
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, wf("network_failed")
	}
	if byteRange != "" && int64(len(b)) != limit {
		return nil, wf("network_failed")
	}
	return b, nil
}
func windowResolveURL(base, rel string) (string, error) {
	b, e := url.Parse(base)
	if e != nil {
		return "", wf("source_unavailable")
	}
	r, e := url.Parse(strings.TrimSpace(rel))
	if e != nil {
		return "", wf("source_unavailable")
	}
	return b.ResolveReference(r).String(), nil
}
func windowRepID(raw string) string { return "rep-" + windowHash([]byte(raw)) }
func windowPeriodID(raw string, index int) string {
	if raw == "" {
		return fmt.Sprintf("period-%d", index)
	}
	return "period-" + windowHash([]byte(raw))
}

var wmDurationPattern = regexp.MustCompile(`^P(?:(\d+)D)?T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?$`)

func wmDuration(s string) (*big.Rat, error) {
	m := wmDurationPattern.FindStringSubmatch(s)
	if m == nil {
		return nil, wf("timestamp_mapping_unavailable")
	}
	r := new(big.Rat)
	for i, mult := range []int64{86400, 3600, 60, 1} {
		if m[i+1] != "" {
			v, ok := new(big.Rat).SetString(m[i+1])
			if !ok {
				return nil, wf("timestamp_mapping_unavailable")
			}
			r.Add(r, new(big.Rat).Mul(v, big.NewRat(mult, 1)))
		}
	}
	return r, nil
}
func wmInherit(list ...*wmTemplate) *wmTemplate {
	o := &wmTemplate{}
	for _, s := range list {
		if s == nil {
			continue
		}
		if s.Timescale != nil {
			o.Timescale = s.Timescale
		}
		if s.PTO != nil {
			o.PTO = s.PTO
		}
		if s.Duration != nil {
			o.Duration = s.Duration
		}
		if s.Start != nil {
			o.Start = s.Start
		}
		if s.Init != "" {
			o.Init = s.Init
		}
		if s.Media != "" {
			o.Media = s.Media
		}
		if s.Timeline != nil {
			o.Timeline = s.Timeline
		}
	}
	return o
}
func wmValue(v *int64, fallback int64) int64 {
	if v == nil {
		return fallback
	}
	return *v
}

var wmToken = regexp.MustCompile(`\$(RepresentationID|Bandwidth|Number|Time)(%0([1-9][0-9]?)d)?\$`)

func wmTemplateURL(base, pattern, id string, bandwidth, number, t int64) (string, error) {
	pattern = strings.ReplaceAll(pattern, "$$", "\x00")
	bad := false
	s := wmToken.ReplaceAllStringFunc(pattern, func(token string) string {
		m := wmToken.FindStringSubmatch(token)
		if m[1] == "RepresentationID" {
			if m[2] != "" {
				bad = true
			}
			return id
		}
		v := bandwidth
		if m[1] == "Number" {
			v = number
		}
		if m[1] == "Time" {
			v = t
		}
		if m[3] != "" {
			width, _ := strconv.Atoi(m[3])
			return fmt.Sprintf("%0*d", width, v)
		}
		return strconv.FormatInt(v, 10)
	})
	if bad || strings.Contains(s, "$") {
		return "", wf("unsupported_capability")
	}
	return windowResolveURL(base, strings.ReplaceAll(s, "\x00", "$"))
}
func parseWindowManifest(ctx context.Context, data []byte, locator string, request windowRequest, fetch wmFetch) ([]wmSpan, *big.Rat, error) {
	var m wmManifest
	if xml.Unmarshal(data, &m) != nil || m.Type == "dynamic" || len(m.Periods) == 0 || len(m.Periods) > 128 {
		return nil, nil, wf("unsupported_capability")
	}
	base, e := windowResolveURL(locator, m.Base)
	if e != nil {
		return nil, nil, e
	}
	var duration *big.Rat
	if m.Duration != "" {
		duration, e = wmDuration(m.Duration)
		if e != nil {
			return nil, nil, e
		}
	}
	spans := []wmSpan{}
	previousEnd := new(big.Rat)
	pinned := ""
	for i, p := range m.Periods {
		start := previousEnd
		if p.Start != "" {
			start, e = wmDuration(p.Start)
			if e != nil {
				return nil, nil, e
			}
		}
		var end *big.Rat
		if p.Duration != "" {
			d, e := wmDuration(p.Duration)
			if e != nil {
				return nil, nil, e
			}
			end = add(start, d)
		} else if i+1 < len(m.Periods) && m.Periods[i+1].Start != "" {
			end, e = wmDuration(m.Periods[i+1].Start)
			if e != nil {
				return nil, nil, e
			}
		} else if i == len(m.Periods)-1 {
			end = duration
		}
		pbase, e := windowResolveURL(base, p.Base)
		if e != nil {
			return nil, nil, e
		}
		type candidate struct {
			rep wmRepresentation
			set wmAdaptation
		}
		candidates := []candidate{}
		for _, a := range p.Sets {
			for _, r := range a.Representations {
				if a.Type == "video" || strings.HasPrefix(a.Mime, "video/") || strings.HasPrefix(r.Mime, "video/") {
					candidates = append(candidates, candidate{r, a})
				}
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			a, b := candidates[i].rep, candidates[j].rep
			if a.Height != b.Height {
				return a.Height > b.Height
			}
			if a.Width != b.Width {
				return a.Width > b.Width
			}
			if a.Bandwidth != b.Bandwidth {
				return a.Bandwidth > b.Bandwidth
			}
			return a.ID < b.ID
		})
		chosen := -1
		for j, c := range candidates {
			if (request.Video.Selection == "highest" && pinned == "") || (request.Video.ID != nil && windowRepID(c.rep.ID) == *request.Video.ID) || (pinned != "" && windowRepID(c.rep.ID) == pinned) {
				chosen = j
				break
			}
		}
		if chosen < 0 {
			return nil, nil, wf("representation_unavailable")
		}
		c := candidates[chosen]
		r, a := c.rep, c.set
		if r.ID == "" {
			return nil, nil, wf("representation_unavailable")
		}
		pinned = windowRepID(r.ID)
		rbase, e := windowResolveURL(pbase, a.Base)
		if e != nil {
			return nil, nil, e
		}
		rbase, e = windowResolveURL(rbase, r.Base)
		if e != nil {
			return nil, nil, e
		}
		span := wmSpan{ID: windowPeriodID(p.ID, i), RepresentationID: pinned, RawID: r.ID, Start: start, End: end}
		protections := append(append([]wmProtection{}, a.Protection...), r.Protection...)
		for _, cp := range protections {
			if cp.PSSH != "" {
				if span.PSSH == "" || strings.Contains(cp.Scheme, widevineSchemeSuffix) {
					span.PSSH = cp.PSSH
				}
			}
		}
		sb := p.SegmentBase
		if a.SegmentBase != nil {
			sb = a.SegmentBase
		}
		if r.SegmentBase != nil {
			sb = r.SegmentBase
		}
		if sb != nil {
			scale := wmValue(sb.Timescale, 1)
			if scale <= 0 {
				return nil, nil, wf("timestamp_mapping_unavailable")
			}
			span.PTO = ticks(wmValue(sb.PTO, 0), scale)
			initURL, e := windowResolveURL(rbase, sb.Init.Source)
			if e != nil {
				return nil, nil, e
			}
			if sb.Init.Range == "" || sb.Index == "" {
				return nil, nil, wf("unsupported_capability")
			}
			span.Init, e = fetch(ctx, initURL, sb.Init.Range, 16<<20)
			if e != nil {
				return nil, nil, e
			}
			index, e := fetch(ctx, rbase, sb.Index, 16<<20)
			if e != nil {
				return nil, nil, e
			}
			offset, _, e := parseByteRange(sb.Index)
			if e != nil || offset < 0 {
				return nil, nil, wf("timestamp_mapping_unavailable")
			}
			reader := bytes.NewReader(index)
			var sidx *mp4.SidxBox
			for reader.Len() > 0 {
				pos := uint64(offset) + uint64(len(index)-reader.Len())
				box, e := mp4.DecodeBox(pos, reader)
				if e != nil {
					return nil, nil, wf("timestamp_mapping_unavailable")
				}
				if s, ok := box.(*mp4.SidxBox); ok {
					if sidx != nil {
						return nil, nil, wf("unsupported_capability")
					}
					sidx = s
				}
			}
			if sidx == nil || sidx.Timescale == 0 || sidx.EarliestPresentationTime > math.MaxInt64 || sidx.AnchorPoint > math.MaxInt64 {
				return nil, nil, wf("timestamp_mapping_unavailable")
			}
			// mp4ff's AnchorPoint already includes first_offset.
			position := int64(sidx.AnchorPoint)
			mediaTime := int64(sidx.EarliestPresentationTime)
			for j, ref := range sidx.SidxRefs {
				if ref.ReferenceType != 0 || ref.ReferencedSize == 0 || ref.SubSegmentDuration == 0 || position > math.MaxInt64-int64(ref.ReferencedSize) || mediaTime > math.MaxInt64-int64(ref.SubSegmentDuration) {
					return nil, nil, wf("unsupported_capability")
				}
				from := add(start, sub(ticks(mediaTime, int64(sidx.Timescale)), span.PTO))
				to := add(from, ticks(int64(ref.SubSegmentDuration), int64(sidx.Timescale)))
				span.Segments = append(span.Segments, wmSegment{rbase, fmt.Sprintf("%d-%d", position, position+int64(ref.ReferencedSize)-1), from, to, int64(j)})
				position += int64(ref.ReferencedSize)
				mediaTime += int64(ref.SubSegmentDuration)
			}
		} else {
			t := wmInherit(p.Template, a.Template, r.Template)
			scale := wmValue(t.Timescale, 1)
			pto := wmValue(t.PTO, 0)
			number := wmValue(t.Start, 1)
			if scale <= 0 || pto < 0 || number < 0 || t.Init == "" || t.Media == "" {
				return nil, nil, wf("unsupported_capability")
			}
			span.PTO = ticks(pto, scale)
			initURL, e := wmTemplateURL(rbase, t.Init, r.ID, r.Bandwidth, number, 0)
			if e != nil {
				return nil, nil, e
			}
			span.Init, e = fetch(ctx, initURL, "", 16<<20)
			if e != nil {
				return nil, nil, e
			}
			entries := []wmTimelineEntry{}
			if t.Timeline != nil {
				entries = t.Timeline.Entries
			} else if t.Duration != nil && end != nil {
				entries = []wmTimelineEntry{{T: &pto, D: *t.Duration, R: -1}}
			} else {
				return nil, nil, wf("timestamp_mapping_unavailable")
			}
			mediaTime := int64(0)
			for j, s := range entries {
				if s.T != nil {
					mediaTime = *s.T
				}
				if s.D <= 0 || mediaTime < 0 || s.R < -1 {
					return nil, nil, wf("timestamp_mapping_unavailable")
				}
				count := s.R + 1
				if s.R == -1 {
					var boundary *big.Rat
					if j+1 < len(entries) && entries[j+1].T != nil {
						boundary = big.NewRat(*entries[j+1].T, 1)
					} else if end != nil {
						boundary = new(big.Rat).Mul(add(sub(end, start), span.PTO), big.NewRat(scale, 1))
					} else {
						return nil, nil, wf("timestamp_mapping_unavailable")
					}
					n := new(big.Rat).Quo(sub(boundary, big.NewRat(mediaTime, 1)), big.NewRat(s.D, 1))
					q := new(big.Int).Quo(n.Num(), n.Denom())
					if new(big.Int).Mod(n.Num(), n.Denom()).Sign() != 0 {
						q.Add(q, big.NewInt(1))
					}
					if !q.IsInt64() {
						return nil, nil, wf("timestamp_mapping_unavailable")
					}
					count = q.Int64()
				}
				if count <= 0 || count > 100000 || len(span.Segments)+int(count) > 100000 {
					return nil, nil, wf("unsupported_capability")
				}
				for k := int64(0); k < count; k++ {
					if mediaTime > math.MaxInt64-s.D || number == math.MaxInt64 {
						return nil, nil, wf("timestamp_mapping_unavailable")
					}
					u, e := wmTemplateURL(rbase, t.Media, r.ID, r.Bandwidth, number, mediaTime)
					if e != nil {
						return nil, nil, e
					}
					from := add(start, ticks(mediaTime-pto, scale))
					to := add(from, ticks(s.D, scale))
					span.Segments = append(span.Segments, wmSegment{u, "", from, to, number})
					mediaTime += s.D
					number++
				}
			}
		}
		if len(span.Segments) == 0 {
			return nil, nil, wf("coverage_incomplete")
		}
		mediaEnd := span.Segments[len(span.Segments)-1].End
		if end == nil {
			span.End = mediaEnd
		} else {
			span.End = minRat(end, mediaEnd)
		}
		if start.Cmp(span.End) >= 0 || start.Cmp(previousEnd) < 0 {
			return nil, nil, wf("timestamp_mapping_unavailable")
		}
		for j, s := range span.Segments {
			if s.Start.Cmp(s.End) >= 0 || (j > 0 && s.Start.Cmp(span.Segments[j-1].End) < 0) {
				return nil, nil, wf("timestamp_mapping_unavailable")
			}
		}
		previousEnd = span.End
		spans = append(spans, span)
	}
	return spans, previousEnd, nil
}

// Snapshot identity is independent of signed CDN URLs. It binds selected source,
// representation, initialization and segment timing/index layout, not a promise
// that a provider will never replace bytes behind unchanged metadata.
func windowSnapshot(req windowRequest, spans []wmSpan) string {
	entries := []any{req.Source.WatchID, req.Source.PresentationID}
	for _, s := range spans {
		segments := []any{}
		for _, seg := range s.Segments {
			segments = append(segments, []any{rs(seg.Start), rs(seg.End), seg.Range, seg.Number})
		}
		entries = append(entries, []any{s.ID, s.RepresentationID, rs(s.Start), rs(s.End), rs(s.PTO), windowHash(s.Init), segments})
	}
	return "snapshot-" + windowHash(windowJSON(entries))
}

func windowAPI(ctx context.Context, locator string, into any) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", locator, nil)
	if e != nil {
		return wf("source_unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+currentAccessToken())
	req.Header.Set("User-Agent", windowUserAgent)
	resp, e := DoRequest(req)
	if e != nil {
		if errors.Is(e, ErrRateLimited) {
			return wf("rate_limited")
		}
		var operation *operationError
		if errors.As(e, &operation) {
			switch operation.code {
			case "network_failed", "authentication_failed":
				return wf(operation.code)
			}
		}
		return wf("source_unavailable")
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if e != nil || json.Unmarshal(b, into) != nil {
		return wf("source_unavailable")
	}
	return nil
}
