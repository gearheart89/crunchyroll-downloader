package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/iyear/gowidevine"
	"github.com/iyear/gowidevine/widevinepb"
)

func windowExample(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("contracts", "media-windows-v1", "examples", name))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func testWindowRequest(t *testing.T) windowRequest {
	t.Helper()
	r, e := parseWindowRequest(windowExample(t, "request-discovery.json"))
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestWindowContractExamples(t *testing.T) {
	for _, pair := range [][2]string{{"request", "request-discovery.json"}, {"request", "request-narrow.json"}, {"result", "result-completed.json"}, {"result", "result-partial.json"}, {"result", "result-failed.json"}, {"capabilities", "capabilities.json"}, {"sample-map", "sample-map-row.jsonl"}, {"progress", "progress-completed.jsonl"}, {"progress", "progress-partial.jsonl"}, {"progress", "progress-invalid-request.jsonl"}} {
		t.Run(pair[1], func(t *testing.T) {
			b := windowExample(t, pair[1])
			rows := [][]byte{b}
			if strings.HasSuffix(pair[1], ".jsonl") {
				rows = bytes.Split(bytes.TrimSpace(b), []byte{'\n'})
			}
			for _, row := range rows {
				if e := validateWindowJSON(pair[0], row); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
func TestWindowRequestRejectsAmbiguity(t *testing.T) {
	b := windowExample(t, "request-discovery.json")
	for name, bad := range map[string][]byte{
		"duplicate":         bytes.Replace(b, []byte(`"version": 1`), []byte(`"version": 1, "version": 1`), 1),
		"escaped duplicate": bytes.Replace(b, []byte(`"version": 1`), []byte(`"version": 1, "\u0076ersion": 1`), 1),
		"unknown":           bytes.Replace(b, []byte(`"version": 1`), []byte(`"version": 1, "surprise": true`), 1),
		"noncanonical":      bytes.Replace(b, []byte(`"300/1"`), []byte(`"600/2"`), 1),
		"negative zero":     bytes.Replace(b, []byte(`"0/1"`), []byte(`"-0/1"`), 1),
		"missing":           bytes.Replace(b, []byte(`"version": 1,`), nil, 1),
		"trailing":          append(append([]byte{}, b...), []byte(`{}`)...),
		"oversized":         bytes.Repeat([]byte{' '}, (1<<20)+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := parseWindowRequest(bad); e == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}
func TestWindowCLIIsolation(t *testing.T) {
	for _, args := range [][]string{{"--capabilities-json"}, {"--request-json", "missing-file"}, {"--url", "SECRET-URL", "--request-json", "missing-file"}, {"--capabilities-json", "--request-json", "missing-file"}} {
		var out bytes.Buffer
		exit := runWindowCLI(args, &out)
		if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "missing-file") {
			t.Fatal("arguments leaked")
		}
		if len(args) == 1 {
			if exit != 0 || validateWindowJSON("capabilities", out.Bytes()) != nil {
				t.Fatalf("bad capabilities: %d %s", exit, out.Bytes())
			}
		} else {
			if exit != 1 || validateWindowJSON("progress", out.Bytes()) != nil {
				t.Fatalf("bad failure: %d %s", exit, out.Bytes())
			}
		}
	}
}
func TestWindowPublication(t *testing.T) {
	if !windowPlatformSupported() {
		t.Skip("media-window publication targets Windows/Linux")
	}
	destination := filepath.Join(t.TempDir(), "new")
	s, e := beginWindowStage(destination, "test-attempt")
	if e != nil {
		t.Fatal(e)
	}
	defer s.close()
	if _, e = beginWindowStage(destination, "other"); e == nil {
		t.Fatal("concurrent acquisition accepted")
	}
	if e = s.removeWork(); e != nil {
		t.Fatal(e)
	}
	if e = s.publish([]byte("{}\n")); e != nil {
		t.Fatal(e)
	}
	if _, e = beginWindowStage(destination, "retry"); e == nil {
		t.Fatal("overwrite accepted")
	}
	if _, e = beginWindowStage("../relative", "bad"); e == nil {
		t.Fatal("relative destination accepted")
	}
}

func TestWindowAtomicPublicationDoesNotReplaceEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "stage")
	destination := filepath.Join(dir, "concurrent-output")
	if e := os.Mkdir(source, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(destination, 0700); e != nil {
		t.Fatal(e)
	}
	if e := publishWindowDirectory(source, destination); e == nil {
		t.Fatal("replaced an existing directory")
	}
	if _, e := os.Stat(source); e != nil {
		t.Fatal("source unexpectedly moved")
	}
	if _, e := os.Stat(destination); e != nil {
		t.Fatal("destination changed")
	}
}

func TestWindowSchemaVocabularyIsImplemented(t *testing.T) {
	allowed := map[string]bool{}
	for _, s := range strings.Fields("$schema $id title $defs $ref type properties required additionalProperties const enum oneOf allOf if then else items minItems maxItems uniqueItems minLength maxLength pattern minimum maximum format") {
		allowed[s] = true
	}
	names, _ := windowSchemas.ReadDir("contracts/media-windows-v1/schemas")
	for _, name := range names {
		b, _ := windowSchemas.ReadFile("contracts/media-windows-v1/schemas/" + name.Name())
		obj, e := strictJSON(b)
		if e != nil {
			t.Fatal(e)
		}
		var check func(map[string]any)
		check = func(node map[string]any) {
			for k, v := range node {
				if !allowed[k] {
					t.Fatalf("unsupported schema keyword %s in %s", k, name.Name())
				}
				switch k {
				case "$defs", "properties":
					for _, s := range v.(map[string]any) {
						check(s.(map[string]any))
					}
				case "oneOf", "allOf":
					for _, s := range v.([]any) {
						check(s.(map[string]any))
					}
				case "if", "then", "else", "items":
					check(v.(map[string]any))
				}
			}
		}
		check(obj.(map[string]any))
	}
}

func TestWindowCompatiblePeriodsAndConfigurationChange(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(fmt.Sprint(change), func(t *testing.T) {
			f := windowFixture(t, false)
			second := f.spans[0]
			second.ID = "period-1"
			second.Start = rat("12/1")
			second.End = rat("24/1")
			second.Segments = append([]wmSegment{}, second.Segments...)
			for i := range second.Segments {
				second.Segments[i].Start = add(second.Segments[i].Start, rat("12/1"))
				second.Segments[i].End = add(second.Segments[i].End, rat("12/1"))
			}
			if change {
				init, e := mp4.DecodeFile(bytes.NewReader(second.Init))
				if e != nil {
					t.Fatal(e)
				}
				init.Init.Moov.Traks[0].Mdia.Minf.Stbl.Stsd.AvcX.AvcC.AVCLevelIndication++
				var b bytes.Buffer
				init.Init.Encode(&b)
				second.Init = b.Bytes()
			}
			f.spans = append(f.spans, second)
			f.duration = rat("24/1")
			req := testWindowRequest(t)
			req.Windows = req.Windows[:1]
			req.Windows[0].Start = "10/1"
			req.Windows[0].End = "14/1"
			req.Delivery.Before = "1/1"
			req.Delivery.After = "1/1"
			result := fixtureWindowResult(req, "attempt", f.duration)
			e := acquireWindowSpans(context.Background(), req, f.spans, f.duration, t.TempDir(), &windowReporter{out: io.Discard, attempt: "attempt", request: req.RequestID}, result, f.fetch, func(int) ([]*widevine.Key, error) { return nil, nil })
			if e != nil {
				t.Fatal(e)
			}
			finalizeWindowResult(result)
			want := "completed"
			clips := 2
			if change {
				want = "partial"
				clips = 1
				if !bytes.Contains(windowJSON(result), []byte("codec_configuration_changed")) {
					t.Fatalf("missing typed configuration error: %s", windowJSON(result))
				}
			}
			if result["status"] != want || len(result["clips"].([]any)) != clips {
				t.Fatalf("bad split result: %s", windowJSON(result))
			}
		})
	}
}

type wmFixture struct {
	spans      []wmSpan
	duration   *big.Rat
	assets     map[string][]byte
	requests   []string
	sourceHash string
}

func windowFixture(t *testing.T, openGOP bool) *wmFixture {
	t.Helper()
	ff, _ := requireMediaTools(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mp4")
	params := "keyint=24:min-keyint=24:scenecut=0:bframes=2"
	if openGOP {
		params += ":open-gop=1"
	}
	runMediaTool(t, ff, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=96x64:rate=24:duration=12", "-an", "-c:v", "libx264", "-preset", "veryfast", "-x264-params", params, "-movflags", "+empty_moov+frag_keyframe+default_base_moof+negative_cts_offsets", "-f", "mp4", path)
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	f, e := mp4.DecodeFile(bytes.NewReader(b))
	if e != nil {
		t.Fatal(e)
	}
	var init bytes.Buffer
	if e = f.Init.Encode(&init); e != nil {
		t.Fatal(e)
	}
	v, e := inspectWindowInit(init.Bytes())
	if e != nil {
		t.Fatal(e)
	}
	fixture := &wmFixture{assets: map[string][]byte{}, sourceHash: windowHash(b)}
	span := wmSpan{ID: "period-0", RepresentationID: windowRepID("video/1080"), RawID: "video/1080", Start: rat("0/1"), PTO: rat("0/1"), Init: init.Bytes()}
	var all []mp4.FullSample
	for _, seg := range f.Segments {
		for _, frag := range seg.Fragments {
			ss, e := frag.GetFullSamples(f.Init.Moov.Mvex.Trex)
			if e != nil {
				t.Fatal(e)
			}
			all = append(all, ss...)
		}
	}
	// Repack as one-second delivery chunks, intentionally independent from
	// random-access boundaries when open GOP is enabled.
	for i := 0; i < len(all); i += 24 {
		end := i + 24
		if end > len(all) {
			end = len(all)
		}
		frag, _ := mp4.CreateFragment(uint32(i/24+1), v.Track.Tkhd.TrackID)
		var encoded bytes.Buffer
		for _, s := range all[i:end] {
			frag.AddFullSample(s)
		}
		if e = frag.Encode(&encoded); e != nil {
			t.Fatal(e)
		}
		name := fmt.Sprintf("https://fixture.invalid/seg-%d", i/24)
		fixture.assets[name] = encoded.Bytes()
		span.Segments = append(span.Segments, wmSegment{URL: name, Start: ticks(int64(all[i].DecodeTime), v.Scale), End: ticks(int64(all[end-1].DecodeTime)+int64(all[end-1].Dur), v.Scale), Number: int64(i / 24)})
	}
	span.End = span.Segments[len(span.Segments)-1].End
	fixture.duration = span.End
	fixture.spans = []wmSpan{span}
	return fixture
}
func (f *wmFixture) fetch(ctx context.Context, u, r string, limit int64) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, wf("cancelled")
	}
	f.requests = append(f.requests, u)
	b, ok := f.assets[u]
	if !ok {
		return nil, wf("network_failed")
	}
	return b, nil
}
func fixtureWindowResult(req windowRequest, attempt string, duration *big.Rat) map[string]any {
	r := newWindowResult(req, attempt)
	r["source"] = map[string]any{"provider": "crunchyroll", "watch_id": req.Source.WatchID, "presentation_id": req.Source.PresentationID, "presentation_locale": req.Source.Locale, "provider_revision": nil, "snapshot_id": "fixture", "episode_duration_seconds": rs(duration)}
	return r
}
func TestWindowAcquisitionKeepsSamplesAndSkipsMiddle(t *testing.T) {
	f := windowFixture(t, false)
	req := testWindowRequest(t)
	req.Windows[0].End = "2/1"
	req.Windows[1].Start = "-2/1"
	req.Delivery.Before = "1/1"
	req.Delivery.After = "1/1"
	stage := t.TempDir()
	result := fixtureWindowResult(req, "attempt", f.duration)
	var stream bytes.Buffer
	reporter := &windowReporter{out: &stream, attempt: "attempt", request: req.RequestID}
	e := acquireWindowSpans(context.Background(), req, f.spans, f.duration, stage, reporter, result, f.fetch, func(int) ([]*widevine.Key, error) { return nil, nil })
	if e != nil {
		t.Fatal(e)
	}
	finalizeWindowResult(result)
	if result["status"] != "completed" {
		t.Fatalf("acquisition: %s", windowJSON(result))
	}
	if len(result["clips"].([]any)) != 2 {
		t.Fatalf("expected two independent clips: %s", windowJSON(result))
	}
	if e = validateWindowJSON("result", windowJSON(result)); e != nil {
		t.Fatal(e)
	}
	for _, u := range f.requests {
		if strings.HasSuffix(u, "seg-5") {
			t.Fatal("downloaded unneeded middle segment")
		}
	}
	for _, item := range result["clips"].([]any) {
		clip := item.(map[string]any)
		timeline := clip["timeline"].(map[string]any)
		b, e := os.ReadFile(filepath.Join(stage, timeline["path"].(string)))
		if e != nil {
			t.Fatal(e)
		}
		if windowHash(b) != timeline["sha256"] {
			t.Fatal("sidecar hash mismatch")
		}
		for i, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
			if e = validateWindowJSON("sample-map", line); e != nil {
				t.Fatal(e)
			}
			var row map[string]any
			json.Unmarshal(line, &row)
			if row["sample_index"] != float64(i) || row["source_sample_sha256"] != row["clip_sample_sha256"] {
				t.Fatal("invalid sample evidence")
			}
		}
	}
}

func TestWindowClipValidation(t *testing.T) {
	f := windowFixture(t, false)
	span := f.spans[0]
	v, e := loadWindowSamples(context.Background(), span, []int{0, 1, 2, 3}, nil, f.fetch)
	if e != nil {
		t.Fatal(e)
	}
	ss, e := selectWindowSamples(v, span, wmInterval{rat("0/1"), rat("2/1")}, rat("1/1"), rat("1/1"), false)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "clip.mp4")
	_, e = writeWindowClip(context.Background(), v, ss, path, "clip", []wmInterval{{rat("0/1"), rat("2/1")}}, nil)
	if e != nil {
		_, probe := requireMediaTools(t)
		t.Log(string(runMediaTool(t, probe, "-v", "error", "-show_streams", "-show_packets", "-read_intervals", "%+#3", "-show_data_hash", "sha256", "-of", "json", path)))
		t.Logf("source first sample: pts %d dts %d duration %d hash %s scale %d size %dx%d", ss[0].PTS, ss[0].DecodeTime, ss[0].Dur, windowHash(ss[0].Data), v.Scale, v.Entry.Width, v.Entry.Height)
		t.Fatal(e)
	}
}
func TestWindowBoundaryPolicyAndOverlap(t *testing.T) {
	req := testWindowRequest(t)
	spans := []wmSpan{{Start: rat("0/1"), End: rat("90/1")}}
	result := newWindowResult(req, "attempt")
	windows := resolveWindowRequests(req, rat("90/1"), spans, result)
	if len(windows) != 2 || rs(windows[0].Content.End) != "90/1" || rs(windows[1].Content.Start) != "0/1" {
		t.Fatal("short discovery not clamped")
	}
	if len(planWindowGroups(req, windows, spans)) != 1 {
		t.Fatal("overlap was not coalesced")
	}
	req.Windows[0].Policy = "require_requested_interval"
	result = newWindowResult(req, "attempt")
	windows = resolveWindowRequests(req, rat("90/1"), spans, result)
	if len(windows) != 1 || len(result["errors"].([]any)) != 1 {
		t.Fatal("strict interval silently clamped")
	}
}
func TestWindowOpenGOPCannotPretendToBeRandomAccess(t *testing.T) {
	f := windowFixture(t, true)
	span := f.spans[0]
	indexes := []int{4, 5, 6, 7}
	v, e := loadWindowSamples(context.Background(), span, indexes, nil, f.fetch)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = selectWindowSamples(v, span, wmInterval{rat("5/1"), rat("6/1")}, rat("1/1"), rat("1/1"), false); e == nil {
		t.Fatal("non-IDR open GOP accepted as self-contained")
	}
}
func TestWindowCancelledAcquisition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := testWindowRequest(t)
	result := newWindowResult(req, "attempt")
	e := acquireWindowSpans(ctx, req, []wmSpan{{Start: rat("0/1"), End: rat("500/1")}}, rat("500/1"), t.TempDir(), &windowReporter{out: io.Discard}, result, nil, nil)
	if windowErrorCode(e) != "cancelled" {
		t.Fatal(e)
	}
}

func TestWindowEncryptedSegmentsKeepOriginalSamples(t *testing.T) {
	f := windowFixture(t, false)
	span := f.spans[0]
	original, e := loadWindowSamples(context.Background(), span, []int{0, 1, 2}, nil, f.fetch)
	if e != nil {
		t.Fatal(e)
	}
	init, e := mp4.DecodeFile(bytes.NewReader(span.Init))
	if e != nil {
		t.Fatal(e)
	}
	key := bytes.Repeat([]byte{0x42}, 16)
	kid := mp4.UUID(bytes.Repeat([]byte{0x24}, 16))
	iv := bytes.Repeat([]byte{0x13}, 16)
	protect, e := mp4.InitProtect(init.Init, key, iv, "cenc", kid, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, seg := range span.Segments {
		file, e := mp4.DecodeFile(bytes.NewReader(f.assets[seg.URL]))
		if e != nil {
			t.Fatal(e)
		}
		var b bytes.Buffer
		for _, s := range file.Segments {
			for _, frag := range s.Fragments {
				if e = mp4.EncryptFragment(frag, key, iv, protect); e != nil {
					t.Fatal(e)
				}
				if e = frag.Encode(&b); e != nil {
					t.Fatal(e)
				}
			}
		}
		f.assets[seg.URL] = b.Bytes()
	}
	var encodedInit bytes.Buffer
	if e = init.Init.Encode(&encodedInit); e != nil {
		t.Fatal(e)
	}
	span.Init = encodedInit.Bytes()
	keys := []*widevine.Key{{ID: []byte(kid), Key: key, Type: widevinepb.License_KeyContainer_CONTENT}}
	got, e := loadWindowSamples(context.Background(), span, []int{0, 1, 2}, keys, f.fetch)
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Samples) != len(original.Samples) {
		t.Fatal("sample loss")
	}
	for i, s := range got.Samples {
		if s.PTS != original.Samples[i].PTS || !bytes.Equal(s.Data, original.Samples[i].Data) {
			t.Fatal("decryption changed encoded sample or timing")
		}
	}
}
func TestWindowPartialFailureKeepsOnlyValidatedClips(t *testing.T) {
	f := windowFixture(t, false)
	req := testWindowRequest(t)
	req.Windows[0].End = "2/1"
	req.Windows[1].Start = "-2/1"
	req.Delivery.Before = "1/1"
	req.Delivery.After = "1/1"
	delete(f.assets, f.spans[0].Segments[10].URL)
	result := fixtureWindowResult(req, "attempt", f.duration)
	reporter := &windowReporter{out: io.Discard, attempt: "attempt", request: req.RequestID}
	e := acquireWindowSpans(context.Background(), req, f.spans, f.duration, t.TempDir(), reporter, result, f.fetch, func(int) ([]*widevine.Key, error) { return nil, nil })
	if e != nil {
		t.Fatal(e)
	}
	finalizeWindowResult(result)
	if result["status"] != "partial" || len(result["clips"].([]any)) != 1 || validateWindowJSON("result", windowJSON(result)) != nil {
		t.Fatalf("invalid partial result: %s", windowJSON(result))
	}
}

func TestWindowCLICompletedPartialFailedPublication(t *testing.T) {
	if !windowPlatformSupported() {
		t.Skip("media-window CLI targets Windows/Linux")
	}
	f := windowFixture(t, false)
	for _, mode := range []string{"completed", "partial", "failed"} {
		t.Run(mode, func(t *testing.T) {
			var request map[string]any
			json.Unmarshal(windowExample(t, "request-discovery.json"), &request)
			dir := t.TempDir()
			output := filepath.Join(dir, "acquisition")
			request["output"].(map[string]any)["directory"] = output
			ws := request["windows"].([]any)
			ws[0].(map[string]any)["end_seconds"] = "2/1"
			ws[1].(map[string]any)["start_seconds"] = "-2/1"
			delivery := request["delivery"].(map[string]any)
			delivery["max_dependency_padding_before_seconds"] = "1/1"
			delivery["max_dependency_padding_after_seconds"] = "1/1"
			path := filepath.Join(dir, "request.json")
			os.WriteFile(path, windowJSON(request), 0600)
			runner := func(ctx context.Context, req windowRequest, stage string, r *windowReporter, result map[string]any, options cvaAuthOptions) error {
				if mode == "failed" {
					return wf("source_unavailable")
				}
				result["source"] = fixtureWindowResult(req, r.attempt, f.duration)["source"]
				fetch := func(ctx context.Context, u, br string, limit int64) ([]byte, error) {
					if mode == "partial" && strings.HasSuffix(u, "seg-10") {
						return nil, wf("network_failed")
					}
					return f.fetch(ctx, u, br, limit)
				}
				return acquireWindowSpans(ctx, req, f.spans, f.duration, stage, r, result, fetch, func(int) ([]*widevine.Key, error) { return nil, nil })
			}
			var stream bytes.Buffer
			exit := runWindowCLIWith([]string{"--request-json", path}, &stream, runner)
			if (exit == 0) != (mode == "completed") {
				t.Fatalf("bad exit %d: %s", exit, stream.String())
			}
			lines := bytes.Split(bytes.TrimSpace(stream.Bytes()), []byte{'\n'})
			var last map[string]any
			for i, line := range lines {
				if validateWindowJSON("progress", line) != nil {
					t.Fatalf("invalid event: %s", line)
				}
				json.Unmarshal(line, &last)
				if last["sequence"] != float64(i+1) {
					t.Fatal("sequence gap")
				}
				if i < len(lines)-1 && last["event"] == "terminal" {
					t.Fatal("early terminal")
				}
			}
			if last["event"] != "terminal" || last["status"] != mode {
				t.Fatalf("bad terminal: %s", stream.String())
			}
			b, e := os.ReadFile(filepath.Join(output, "result.json"))
			if e != nil {
				t.Fatal(e)
			}
			if windowHash(b) != last["result"].(map[string]any)["sha256"] || validateWindowJSON("result", b) != nil {
				t.Fatal("result mismatch")
			}
			if _, e = os.Stat(filepath.Join(output, "work")); !os.IsNotExist(e) {
				t.Fatal("temporary downloads published")
			}
			stream.Reset()
			if runWindowCLIWith([]string{"--request-json", path}, &stream, runner) != 1 {
				t.Fatal("existing acquisition overwritten")
			}
		})
	}
}

func TestWindowVariableTimingAndSourceOrigin(t *testing.T) {
	f := windowFixture(t, false)
	span := &f.spans[0]
	oldInit, e := inspectWindowInit(span.Init)
	if e != nil {
		t.Fatal(e)
	}
	scale := oldInit.Scale
	origin := uint64(10 * scale)
	span.PTO = ticks(int64(origin), scale)
	decode := origin
	for i, segment := range span.Segments {
		file, e := mp4.DecodeFile(bytes.NewReader(f.assets[segment.URL]))
		if e != nil {
			t.Fatal(e)
		}
		frag := file.Segments[0].Fragments[0]
		ss, e := frag.GetFullSamples(oldInit.Init.Moov.Mvex.Trex)
		if e != nil {
			t.Fatal(e)
		}
		out, _ := mp4.CreateFragment(uint32(i+1), oldInit.Track.Tkhd.TrackID)
		lo := new(big.Rat).SetInt64(1000000)
		hi := new(big.Rat)
		for _, s := range ss {
			frame := (int64(s.DecodeTime) + int64(s.CompositionTimeOffset)) / 512
			presentation := int64(origin) + (frame/2)*1536 + (frame%2)*512
			s.DecodeTime = decode
			s.CompositionTimeOffset = int32(presentation - int64(decode))
			s.Dur = uint32(512 * (1 + frame%2))
			decode += uint64(s.Dur)
			out.AddFullSample(s)
			a := ticks(presentation-int64(origin), scale)
			lo = minRat(lo, a)
			hi = maxRat(hi, add(a, ticks(int64(s.Dur), scale)))
		}
		var b bytes.Buffer
		if e = out.Encode(&b); e != nil {
			t.Fatal(e)
		}
		f.assets[segment.URL] = b.Bytes()
		span.Segments[i].Start = lo
		span.Segments[i].End = hi
	}
	span.End = ticks(int64(decode-origin), scale)
	f.duration = span.End
	req := testWindowRequest(t)
	req.Windows = req.Windows[:1]
	req.Windows[0].End = "3/1"
	req.Delivery.Before = "1/1"
	req.Delivery.After = "2/1"
	result := fixtureWindowResult(req, "attempt", f.duration)
	reporter := &windowReporter{out: io.Discard, attempt: "attempt", request: req.RequestID}
	e = acquireWindowSpans(context.Background(), req, f.spans, f.duration, t.TempDir(), reporter, result, f.fetch, func(int) ([]*widevine.Key, error) { return nil, nil })
	if e != nil {
		t.Fatal(e)
	}
	finalizeWindowResult(result)
	if result["status"] != "completed" {
		t.Fatalf("VFR acquisition failed: %s", windowJSON(result))
	}
}

func TestWindowNegativeLeadingPTSRemainsDecodePadding(t *testing.T) {
	f := windowFixture(t, false)
	for u, b := range f.assets {
		file, e := mp4.DecodeFile(bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		var encoded bytes.Buffer
		for _, seg := range file.Segments {
			for _, frag := range seg.Fragments {
				for i := range frag.Moof.Traf.Trun.Samples {
					frag.Moof.Traf.Trun.Samples[i].CompositionTimeOffset -= 512
				}
				if e = frag.Encode(&encoded); e != nil {
					t.Fatal(e)
				}
			}
		}
		f.assets[u] = encoded.Bytes()
	}
	req := testWindowRequest(t)
	req.Windows = req.Windows[:1]
	req.Windows[0].End = "2/1"
	req.Delivery.Before = "1/1"
	req.Delivery.After = "1/1"
	stage := t.TempDir()
	result := fixtureWindowResult(req, "attempt", f.duration)
	e := acquireWindowSpans(context.Background(), req, f.spans, f.duration, stage, &windowReporter{out: io.Discard, attempt: "attempt", request: req.RequestID}, result, f.fetch, func(int) ([]*widevine.Key, error) { return nil, nil })
	if e != nil {
		t.Fatal(e)
	}
	finalizeWindowResult(result)
	if result["status"] != "completed" {
		t.Fatalf("negative PTS rejected: %s", windowJSON(result))
	}
	clip := result["clips"].([]any)[0].(map[string]any)
	data, e := os.ReadFile(filepath.Join(stage, clip["timeline"].(map[string]any)["path"].(string)))
	if e != nil {
		t.Fatal(e)
	}
	var first map[string]any
	json.Unmarshal(bytes.SplitN(data, []byte{'\n'}, 2)[0], &first)
	if first["source_pts"] != "-512" || first["episode_time_seconds"] != "-1/24" || first["role"] != "decode_dependency" {
		t.Fatalf("lost negative source origin: %s", windowJSON(first))
	}
}

func TestWindowRequiresSelfContainedInit(t *testing.T) {
	f := windowFixture(t, false)
	init, e := mp4.DecodeFile(bytes.NewReader(f.spans[0].Init))
	if e != nil {
		t.Fatal(e)
	}
	dref := init.Init.Moov.Traks[0].Mdia.Minf.Dinf.Dref.Children[0].(*mp4.URLBox)
	dref.Flags = 0
	dref.Location = "file:///external-media.mp4"
	dref.NoLocation = false
	var b bytes.Buffer
	init.Init.Encode(&b)
	if _, e = inspectWindowInit(b.Bytes()); windowErrorCode(e) != "unsupported_capability" {
		t.Fatal("external data reference accepted", e)
	}
}

func TestWindowSingleEditMapsCompositionTimeToEpisode(t *testing.T) {
	f := windowFixture(t, false)
	span := &f.spans[0]
	baseline, e := loadWindowSamples(context.Background(), *span, []int{0, 1, 2}, nil, f.fetch)
	if e != nil {
		t.Fatal(e)
	}
	const offset = 1024
	init, e := mp4.DecodeFile(bytes.NewReader(span.Init))
	if e != nil {
		t.Fatal(e)
	}
	edts := &mp4.EdtsBox{}
	edts.AddChild(&mp4.ElstBox{Entries: []mp4.ElstEntry{{SegmentDuration: uint64(12 * init.Init.Moov.Mvhd.Timescale), MediaTime: offset, MediaRateInteger: 1}}})
	edts.Elst = []*mp4.ElstBox{edts.Children[0].(*mp4.ElstBox)}
	init.Init.Moov.Traks[0].AddChild(edts)
	var encoded bytes.Buffer
	if e = init.Init.Encode(&encoded); e != nil {
		t.Fatal(e)
	}
	span.Init = append([]byte{}, encoded.Bytes()...)
	for u, b := range f.assets {
		file, e := mp4.DecodeFile(bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		encoded.Reset()
		for _, s := range file.Segments {
			for _, frag := range s.Fragments {
				for _, trun := range frag.Moof.Traf.Truns {
					for i := range trun.Samples {
						trun.Samples[i].CompositionTimeOffset += offset
					}
				}
				if e = frag.Encode(&encoded); e != nil {
					t.Fatal(e)
				}
			}
		}
		f.assets[u] = append([]byte{}, encoded.Bytes()...)
	}
	v, e := loadWindowSamples(context.Background(), *span, []int{0, 1, 2}, nil, f.fetch)
	if e != nil {
		t.Fatal(e)
	}
	for i, s := range v.Samples {
		old := baseline.Samples[i]
		if s.PTS != old.PTS+offset || s.DecodeTime != old.DecodeTime || s.Episode.Cmp(old.Episode) != 0 || !bytes.Equal(s.Data, old.Data) {
			t.Fatal("edit corrupted raw timing, episode mapping, or payload")
		}
	}
	selected, e := selectWindowSamples(v, *span, wmInterval{rat("0/1"), rat("2/1")}, rat("1/1"), rat("1/1"), false)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "edited.mp4")
	if _, e = writeWindowClip(context.Background(), v, selected, path, "clip", []wmInterval{{rat("0/1"), rat("2/1")}}, nil); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(strings.TrimSuffix(path, ".mp4") + ".timeline.jsonl")
	if e != nil {
		t.Fatal(e)
	}
	var row map[string]any
	json.Unmarshal(bytes.SplitN(b, []byte{'\n'}, 2)[0], &row)
	if row["source_pts"] != "1024" || row["source_dts"] != "0" || row["episode_time_seconds"] != "0/1" {
		t.Fatal("raw and edited timelines not distinguished")
	}
	v.EditDuration = rat("1/1")
	if _, e = selectWindowSamples(v, *span, wmInterval{rat("0/1"), rat("2/1")}, rat("1/1"), rat("1/1"), false); windowErrorCode(e) != "coverage_incomplete" {
		t.Fatal("content outside finite edit accepted")
	}
	for _, edit := range []mp4.ElstEntry{{MediaTime: -1, MediaRateInteger: 1}, {MediaTime: 0, MediaRateInteger: 2}, {MediaTime: 0, MediaRateInteger: 1, MediaRateFraction: 1}} {
		edts.Elst[0].Entries = []mp4.ElstEntry{edit}
		encoded.Reset()
		init.Init.Encode(&encoded)
		if _, e = inspectWindowInit(encoded.Bytes()); windowErrorCode(e) != "timestamp_mapping_unavailable" {
			t.Fatal("unsupported edit accepted")
		}
	}
	edts.Elst[0].Entries = []mp4.ElstEntry{{MediaRateInteger: 1}, {MediaRateInteger: 1}}
	encoded.Reset()
	init.Init.Encode(&encoded)
	if _, e = inspectWindowInit(encoded.Bytes()); windowErrorCode(e) != "timestamp_mapping_unavailable" {
		t.Fatal("multiple edits accepted")
	}
}

func TestWindowCoverageReorderedRoundingDoesNotHideMissingSamples(t *testing.T) {
	// Display order differs from decode order. One tick of integer rounding is
	// paired with a different frame duration after the B pictures are reordered.
	makeSamples := func() []wmSample {
		return []wmSample{
			{FullSample: mp4.FullSample{Sample: mp4.Sample{Dur: 417083}, DecodeTime: 0}, Episode: ticks(0, 10000000)},
			{FullSample: mp4.FullSample{Sample: mp4.Sample{Dur: 417084}, DecodeTime: 417083}, Episode: ticks(834167, 10000000)},
			{FullSample: mp4.FullSample{Sample: mp4.Sample{Dur: 417083}, DecodeTime: 834167}, Episode: ticks(417084, 10000000)},
		}
	}
	want := wmInterval{ticks(0, 10000000), ticks(1251250, 10000000)}
	if !wmCovered(want, makeSamples(), 10000000) {
		t.Fatal("integer clock rounding rejected")
	}
	for _, mode := range []string{"missing", "two-tick-gap", "before-start", "after-end", "duplicate-pts"} {
		t.Run(mode, func(t *testing.T) {
			s := makeSamples()
			w := want
			switch mode {
			case "missing":
				s = append(s[:1], s[2:]...)
			case "two-tick-gap":
				s[2].Episode = ticks(417085, 10000000)
			case "before-start":
				w.Start = ticks(-1, 10000000)
			case "after-end":
				w.End = ticks(1251252, 10000000)
			case "duplicate-pts":
				s[2].Episode = s[0].Episode
			}
			if wmCovered(w, s, 10000000) {
				t.Fatal("invalid coverage accepted")
			}
		})
	}
}
