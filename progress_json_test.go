package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unki2aut/go-mpd"
)

// Validate the consumer's contract against bytes, not internal event objects.
func checkProtocol(t *testing.T, data []byte, exit int) []map[string]any {
	t.Helper()
	if len(data) == 0 || data[len(data)-1] != '\n' || bytes.ContainsAny(data, "\r\x1b") {
		t.Fatalf("invalid JSONL framing: %q", data)
	}
	var records []map[string]any
	terminal, attempt := 0, 0
	lastElapsed := float64(0)
	tracks := map[string]map[string]any{}
	percentages := map[string]float64{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var e map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("non-JSON stdout: %s: %v", scanner.Text(), err)
		}
		if terminal != 0 {
			t.Fatal("event after terminal record")
		}
		if e["protocol"] != "cva-downloader-progress" || e["version"] != float64(1) || e["sequence"] != float64(len(records)) {
			t.Fatalf("bad envelope: %v", e)
		}
		elapsed, ok := e["elapsed_ms"].(float64)
		if !ok || elapsed < lastElapsed || elapsed != math.Trunc(elapsed) {
			t.Fatalf("bad elapsed time: %v", e)
		}
		lastElapsed = elapsed
		if len(records) == 0 && (e["event"] != "protocol" || e["tool"] != "crunchyroll-downloader") {
			t.Fatal("missing initial protocol")
		}
		switch e["event"] {
		case "protocol":
			if len(records) != 0 {
				t.Fatal("duplicate protocol")
			}
		case "phase":
			if !strings.Contains("|initializing|authenticating|resolving|downloading|decrypting|muxing|finalizing|cleanup|", "|"+fmt.Sprint(e["phase"])+"|") {
				t.Fatalf("bad phase: %v", e)
			}
		case "attempt_started":
			attempt++
			if e["attempt"] != float64(attempt) {
				t.Fatal("invalid attempt order")
			}
			tracks, percentages = map[string]map[string]any{}, map[string]float64{}
		case "track_started", "track_progress":
			id, ok := e["track_id"].(string)
			if !ok || id == "" || e["attempt"] != float64(attempt) {
				t.Fatalf("bad track identity: %v", e)
			}
			if _, present := e["locale"]; !present {
				t.Fatal("locale missing")
			}
			if e["kind"] != "video" && e["kind"] != "audio" {
				t.Fatal("non AV track")
			}
			if e["kind"] == "audio" && (e["locale"] == nil || e["locale"] == "") {
				t.Fatal("audio without locale")
			}
			if e["event"] == "track_started" {
				if tracks[id] != nil {
					t.Fatal("duplicate track start")
				}
				tracks[id] = e
			} else {
				if tracks[id] == nil || tracks[id]["kind"] != e["kind"] || tracks[id]["locale"] != e["locale"] {
					t.Fatal("progress without matching start")
				}
				p, ok := e["percent"].(float64)
				if !ok || p < percentages[id] || p > 100 || p != math.Trunc(p) {
					t.Fatalf("bad percent: %v", e)
				}
				percentages[id] = p
				for _, key := range []string{"segments_current", "segments_total", "mbps"} {
					if _, ok := e[key]; !ok {
						t.Fatalf("missing %s", key)
					}
				}
				if e["segments_total"] != nil {
					total, tok := e["segments_total"].(float64)
					current, cok := e["segments_current"].(float64)
					if !tok || !cok || total <= 0 || current < 0 || current > total || total != math.Trunc(total) || current != math.Trunc(current) {
						t.Fatal("invalid segments")
					}
				} else if e["segments_current"] != nil {
					t.Fatal("mixed segment nulls")
				}
				if rate, ok := e["mbps"].(float64); e["mbps"] != nil && (!ok || rate < 0 || math.IsInf(rate, 0) || math.IsNaN(rate)) {
					t.Fatal("invalid rate")
				}
			}
		case "attempt_failed":
			if e["attempt"] != float64(attempt) || e["recoverable"] != true {
				t.Fatal("bad recoverable failure")
			}
		case "retry_wait":
			if len(records) == 0 || records[len(records)-1]["event"] != "attempt_failed" || e["next_attempt"] != float64(attempt+1) || e["delay_seconds"].(float64) < 1 {
				t.Fatal("bad retry")
			}
		case "warning":
			if e["display_message"] == nil {
				t.Fatal("warning without message")
			}
		case "completed", "skipped_existing", "failed":
			terminal++
			if e["event"] == "failed" {
				if exit == 0 || e["display_message"] == nil || e["code"] == nil || e["phase"] == nil {
					t.Fatal("invalid failure terminal")
				}
			} else if exit != 0 || e["output_path"] == nil {
				t.Fatal("invalid success terminal")
			}
		default:
			t.Fatalf("unknown event: %v", e)
		}
		records = append(records, e)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if terminal != 1 {
		t.Fatalf("got %d terminals", terminal)
	}
	return records
}

func captureEvents(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := events
	events = newEventReporter(&buf, time.Now())
	t.Cleanup(func() { events = previous })
	return &buf
}

func TestJSONConcurrentProgressAndRetry(t *testing.T) {
	buf := captureEvents(t)
	previous := backoff
	backoff = newDownloadBackoff(0)
	t.Cleanup(func() { backoff = previous })
	var waits []time.Duration
	backoff.sleep = func(d time.Duration) { waits = append(waits, d) }
	attempt := 0
	err := retryEpisode(func() error {
		attempt++
		events.setPhase("downloading")
		var wg sync.WaitGroup
		for _, track := range []trackIdentity{{id: "video:0", kind: "video"}, {id: "audio:ja-JP", kind: "audio", locale: "ja-JP"}} {
			wg.Add(1)
			go func(track trackIdentity) {
				defer wg.Done()
				events.trackStarted(track)
				bar := newProgressBar("fixture", 100, "segments", track)
				for n := int64(1); n <= 100; n++ {
					bar.update(n, n*1000)
				}
				bar.update(40, 40000) // racing/stale updates must never regress
				if attempt == 2 {
					bar.transferred()
				}
				bar.finish()
			}(track)
		}
		wg.Wait()
		if attempt == 1 {
			events.setPhase("cleanup")
			return ErrRateLimited
		}
		events.output(filepath.Join(t.TempDir(), "Episode.mp4"), false)
		return nil
	})
	status := events.finish(err, "mp4")
	before := buf.Len()
	events.setPhase("cleanup")
	if buf.Len() != before {
		t.Fatal("wrote after terminal")
	}
	records := checkProtocol(t, buf.Bytes(), status)
	if len(waits) != 1 || waits[0] != time.Minute || records[len(records)-1]["attempt"] != float64(2) {
		t.Fatal("retry did not reset attempt")
	}
	for _, e := range records {
		if e["event"] == "track_progress" && e["attempt"] == float64(1) && e["percent"] == float64(100) {
			t.Fatal("unfinished track reported 100")
		}
	}
}

func TestJSONUnknownSizeAndFiniteRates(t *testing.T) {
	buf := captureEvents(t)
	events.startAttempt()
	track := trackIdentity{id: "audio:ja-JP", kind: "audio", locale: "ja-JP"}
	events.trackStarted(track)
	for _, rate := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0, 4.5} {
		events.progress(track, 0, 0, false, false, rate)
	}
	bar := newProgressBar("unknown", 0, "", track)
	bar.transferred()
	bar.finish()
	events.output("episode.mp4", false)
	status := events.finish(nil, "mp4")
	records := checkProtocol(t, buf.Bytes(), status)
	for _, e := range records {
		if e["event"] == "track_progress" && (e["segments_current"] != nil || e["segments_total"] != nil) {
			t.Fatal("single file emitted segment counts")
		}
	}
	if records[len(records)-2]["percent"] != float64(100) {
		t.Fatal("unknown size transfer did not finish")
	}
}

type brokenEventWriter struct{}

func (brokenEventWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestJSONBrokenPipeFails(t *testing.T) {
	r := newEventReporter(brokenEventWriter{}, time.Now())
	r.startAttempt()
	r.output("episode.mp4", false)
	if r.finish(nil, "mp4") == 0 {
		t.Fatal("broken event pipe reported success")
	}
}

type failedBody struct{}

func (failedBody) Read([]byte) (int, error) { return 0, errors.New("private-body-error") }
func (failedBody) Close() error             { return nil }

func TestJSONTransferFailures(t *testing.T) {
	for _, mode := range []string{"segments", "single_file", "unknown_size", "body_error", "network_error", "http_error"} {
		t.Run(mode, func(t *testing.T) {
			buf := captureEvents(t)
			tmp := t.TempDir()
			t.Setenv("TMP", tmp)
			t.Setenv("TEMP", tmp)
			t.Setenv("TMPDIR", tmp)
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = fixtureTransport(func(req *http.Request) (*http.Response, error) {
				if mode == "network_error" {
					return nil, errors.New("private-network-error")
				}
				status, length := http.StatusOK, int64(4)
				var body io.ReadCloser = io.NopCloser(strings.NewReader("junk"))
				if mode == "unknown_size" {
					length = -1
				}
				if mode == "http_error" {
					status = http.StatusServiceUnavailable
				}
				if mode == "body_error" && req.Header.Get("Range") == "bytes=4-" {
					body = failedBody{}
				}
				return &http.Response{StatusCode: status, Body: body, ContentLength: length, Header: make(http.Header), Request: req}, nil
			})
			events.startAttempt()
			events.setPhase("downloading")
			track := trackIdentity{id: "video:0", kind: "video"}
			events.trackStarted(track)
			var err error
			if mode == "segments" {
				var manifest mpd.MPD
				if err := manifest.Decode([]byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet><Representation id="v" height="1080"/><SegmentTemplate initialization="init" media="$Number$"><SegmentTimeline><S d="1" r="2"/></SegmentTimeline></SegmentTemplate></AdaptationSet></Period></MPD>`)); err != nil {
					t.Fatal(err)
				}
				base, rep := "https://fixture.invalid/", "v"
				_, err = downloadParts("video", &base, &rep, manifest.Period[0].AdaptationSets[0], nil, track)
			} else {
				_, err = downloadOnDemandParts("video", onDemandRepresentation{BaseURL: "https://fixture.invalid/video", InitRange: "0-3", IndexRange: "4-7"}, true, nil, track)
			}
			if err == nil {
				t.Fatal("expected invalid encrypted media or transport failure")
			}
			status := events.finish(err, "mp4")
			records := checkProtocol(t, buf.Bytes(), status)
			wantCode := "decryption_failed"
			if mode == "body_error" {
				wantCode = "body_read_failed"
			}
			if mode == "network_error" {
				wantCode = "network_failed"
			}
			if mode == "http_error" {
				wantCode = "response_failed"
			}
			if records[len(records)-1]["code"] != wantCode {
				t.Fatalf("wrong terminal: %v", records[len(records)-1])
			}
			for _, e := range records {
				if e["event"] == "track_progress" {
					if mode == "segments" && e["segments_total"] != float64(3) {
						t.Fatal("wrong segment total")
					}
					if mode != "segments" && e["segments_total"] != nil {
						t.Fatal("single file has segment counts")
					}
					if mode == "body_error" && e["percent"] == float64(100) {
						t.Fatal("failed transfer reported complete")
					}
				}
			}
			files, readErr := os.ReadDir(tmp)
			if readErr != nil || len(files) != 0 {
				t.Fatalf("failed transfer left temporary media: %v, %v", files, readErr)
			}
		})
	}
}

func TestJSONStreamWorkerPanic(t *testing.T) {
	err := streamSegments(io.Discard, []string{"one", "two", "three"}, func(string) ([]byte, error) { panic("fixture") }, nil)
	var op *operationError
	if !errors.As(err, &op) || op.code != "internal_error" {
		t.Fatalf("worker panic did not propagate: %v", err)
	}
}

func TestJSONCredentialFailures(t *testing.T) {
	for _, status := range []string{"credentials_missing", "credential_config", "credential_unavailable", "credential_locked", "credential_corrupt", "credential_store"} {
		var op *operationError
		if !errors.As(cvaStatusError(status), &op) || op.code != "credential_storage_failed" {
			t.Fatalf("wrong classification for %s", status)
		}
	}
}

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Launched as a separate process so stdout bytes and process exit codes are
// exercised together. All HTTP is intercepted; no account or media service is used.
func TestJSONCLIHelper(t *testing.T) {
	mode := os.Getenv("CRDL_TEST_MODE")
	if mode == "" {
		return
	}
	separator := 0
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i + 1
			break
		}
	}
	if separator == 0 {
		os.Exit(90)
	}
	args := os.Args[separator:]
	if strings.HasPrefix(mode, "session_") {
		// A private-pipe helper fixture exercises the real CLI/provider path on
		// both platforms without opening a browser or a real credential store.
		cvaAuthScript = `import json,os,sys
request = json.load(sys.stdin)
operation, mode = request["operation"], os.environ["CRDL_TEST_MODE"]
session = {"cookie":"private-cookie", "device_id":"private-device", "expires_at":0}
if operation == "session-read":
    if mode == "session_locked":
        result = {"status":"credential_locked", "detail":"private-store-error"}
    elif mode == "session_unsupported":
        result = {"status":"session_missing"}
    else:
        if mode == "session_expired":
            session["expires_at"] = 1
        result = {"status":"session_ok", "session":session}
elif operation == "session-refresh":
    if mode == "session_unsupported":
        result = {"status":"session_refresh_unsupported"}
    else:
        assert request["force_refresh"] is True
        result = {"status":"session_ok", "session":session}
elif operation == "session-write":
    assert request["session"]["cookie"] == "private-cookie"
    result = {"status":"session_saved"}
else:
    raise AssertionError("unexpected helper operation")
print(json.dumps(result))`
	}
	if mode == "skip" || mode == "missing_audio" || mode == "cleanup_failure" || mode == "auth_failure" || strings.HasPrefix(mode, "session_") {
		http.DefaultTransport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
			body, status := "", http.StatusOK
			switch {
			case strings.Contains(r.URL.Path, "/auth/"):
				body = `{"access_token":"fixture-token"}`
				if mode == "auth_failure" {
					status = http.StatusForbidden
				}
				if mode == "session_rejected" {
					status = http.StatusUnauthorized
				}
			case strings.Contains(r.URL.Path, "/objects/"):
				body = `{"data":[{"title":"Pilot","episode_metadata":{"audio_locale":"ja-JP","series_title":"Fixture","season_number":1,"episode_number":1}}]}`
			case r.Method == http.MethodDelete:
				status, body = http.StatusInternalServerError, "private-server-error"
			case strings.Contains(r.URL.Path, "/playback/"):
				body = `{"url":"https://fixture.invalid/manifest","token":"private-player-token","subtitles":{"en-US":{"url":"https://fixture.invalid/subtitle","format":"ass"}}}`
			case r.URL.Path == "/manifest":
				body = "not XML: private-server-error"
			default:
				return nil, errors.New("unexpected fixture request")
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
		})
		os.Exit(runCLI(args))
	}
	if mode == "completed" || mode == "warning" || mode == "ffmpeg_failure" || mode == "panic" {
		os.Exit(runCLIWith(args, func() error {
			events.startAttempt()
			events.setPhase("resolving")
			fmt.Println("legacy prose must only reach stderr")
			if mode == "panic" {
				panic("private-server-error")
			}
			dir := os.Getenv("CRDL_TEST_FIXTURE")
			ffmpegPath = os.Getenv("CRDL_TEST_FFMPEG")
			video := filepath.Join(dir, "video.mp4")
			if mode == "ffmpeg_failure" {
				video = filepath.Join(dir, "missing.mp4")
			}
			output := filepath.Join(dir, "Custom 世界.mp4")
			audio := []mediaTrack{{file: filepath.Join(dir, "ja-JP.m4a"), locale: "ja-JP"}, {file: filepath.Join(dir, "en-US.m4a"), locale: "en-US"}}
			subs := []mediaTrack{{file: filepath.Join(dir, "source.srt"), locale: "en-US", format: "srt"}}
			if err := writeOutput(video, audio, subs, output, EpisodeInfo{}); err != nil {
				return err
			}
			events.setPhase("cleanup")
			events.streamOpened()
			if mode == "warning" {
				events.warning("cleanup_failed", "The player stream could not be removed.")
			}
			events.output(output, false)
			return nil
		}))
	}
	os.Exit(runCLI(args))
}

func runJSONProcess(t *testing.T, mode string, args, env []string) ([]map[string]any, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestJSONCLIHelper$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), append([]string{"CRDL_TEST_MODE=" + mode}, env...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		exit = ee.ExitCode()
	}
	if bytes.Contains(stdout.Bytes(), []byte("private-")) {
		t.Fatalf("sensitive diagnostic in JSON: %s", stdout.String())
	}
	return checkProtocol(t, stdout.Bytes(), exit), stderr.String()
}

func TestJSONCLIArgumentFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code string
	}{
		{"unknown", []string{"--bad", "--progress-format", "json"}, "invalid_arguments"},
		{"missing_input", []string{"--progress-format=json"}, "invalid_arguments"},
		{"bad_format", []string{"--progress-format", "json", "--output-format", "avi"}, "invalid_arguments"},
		{"batch", []string{"--progress-format", "json", "--url", "https://www.crunchyroll.com/series/TEST"}, "invalid_arguments"},
		{"auth_check", []string{"--progress-format", "json", "--auth-check"}, "invalid_arguments"},
		{"input_file", []string{"--progress-format", "json", "--file", filepath.Join(t.TempDir(), "missing.txt")}, "input_failed"},
		{"filename", []string{"--progress-format", "json", "--url", "https://www.crunchyroll.com/watch/TEST", "--filename", "../escape"}, "output_configuration_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, _ := runJSONProcess(t, "arguments", tc.args, nil)
			if last := records[len(records)-1]; last["event"] != "failed" || last["code"] != tc.code {
				t.Fatalf("unexpected terminal: %v", last)
			}
		})
	}
}

func TestJSONCLIExistingAndFailures(t *testing.T) {
	// These paths finish before muxing. Satisfy discovery with a deliberately
	// unusable executable so the tests also run on Linux hosts without FFmpeg;
	// an accidental mux attempt will still fail the expected terminal assertion.
	toolDir := t.TempDir()
	toolName := "ffmpeg"
	if runtime.GOOS == "windows" {
		toolName += ".exe"
	}
	if err := os.WriteFile(filepath.Join(toolDir, toolName), []byte("not an executable"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, mode := range []string{"skip", "missing_audio", "cleanup_failure", "auth_failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "episode.mp4")
			if mode == "skip" {
				if err := os.WriteFile(path, []byte("existence only, intentionally invalid MP4"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"--progress-format", "json", "--url", "https://www.crunchyroll.com/watch/TEST", "--etp-rt", "fixture-cookie", "--output-dir", dir, "--filename", "episode", "--output-format", "mp4"}
			if mode == "missing_audio" {
				args = append(args, "--audio-lang", "en-US")
			}
			records, _ := runJSONProcess(t, mode, args, nil)
			last := records[len(records)-1]
			if mode == "skip" {
				if last["event"] != "skipped_existing" || last["output_path"] != path || last["validation"] != "existence_only" {
					t.Fatalf("bad skip: %v", last)
				}
			} else {
				want := map[string]string{"missing_audio": "requested_audio_unavailable", "cleanup_failure": "response_failed", "auth_failure": "authentication_failed"}[mode]
				if last["code"] != want {
					t.Fatalf("got %v, want %s", last, want)
				}
				if mode == "cleanup_failure" {
					found := false
					for _, e := range records {
						if e["event"] == "warning" && e["code"] == "cleanup_failed" {
							found = true
						}
					}
					if !found {
						t.Fatal("missing cleanup warning")
					}
				}
			}
		})
	}
}

func TestJSONCLIMuxTerminals(t *testing.T) {
	ffmpeg, ffprobe := requireMediaTools(t)
	for _, mode := range []string{"completed", "warning", "ffmpeg_failure", "panic"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			makeOutputInputs(t, ffmpeg, dir)
			records, stderr := runJSONProcess(t, mode, []string{"--progress-format", "json", "--output-format", "mp4"}, []string{"CRDL_TEST_FIXTURE=" + dir, "CRDL_TEST_FFMPEG=" + ffmpeg})
			if !strings.Contains(stderr, "legacy prose") {
				t.Fatal("diagnostics not redirected to stderr")
			}
			last := records[len(records)-1]
			if mode == "ffmpeg_failure" || mode == "panic" {
				code := "ffmpeg_failed"
				if mode == "panic" {
					code = "internal_error"
				}
				if last["code"] != code {
					t.Fatalf("bad failure: %v", last)
				}
				if mode == "ffmpeg_failure" && last["phase"] != "muxing" {
					t.Fatal("lost failure phase")
				}
				return
			}
			if last["event"] != "completed" || last["output_format"] != "mp4" {
				t.Fatalf("bad completion: %v", last)
			}
			want := "confirmed"
			if mode == "warning" {
				want = "warning"
			}
			if last["cleanup_status"] != want {
				t.Fatalf("bad cleanup status: %v", last)
			}
			warnings, ok := last["warnings"].([]any)
			if !ok || (mode == "warning" && (len(warnings) != 1 || warnings[0] != "cleanup_failed")) || (mode == "completed" && len(warnings) != 0) {
				t.Fatal("invalid warning list")
			}
			var probe outputProbe
			if err := json.Unmarshal(runMediaTool(t, ffprobe, "-v", "error", "-show_streams", "-of", "json", last["output_path"].(string)), &probe); err != nil {
				t.Fatal(err)
			}
			if len(probe.Streams) != 3 {
				t.Fatalf("want video and two audio tracks: %+v", probe.Streams)
			}
			for _, stream := range probe.Streams {
				if stream.CodecType != "video" && stream.CodecType != "audio" {
					t.Fatal("subtitle embedded in JSON mode")
				}
			}
			sidecars, _ := filepath.Glob(filepath.Join(dir, "Custom 世界.*.srt"))
			staging, _ := filepath.Glob(filepath.Join(dir, ".crdl-output-*"))
			if len(sidecars) != 0 || len(staging) != 0 {
				t.Fatal("sidecar or unfinished staging output")
			}
		})
	}
}
