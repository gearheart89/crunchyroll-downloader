package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

var processStarted = time.Now()

// events is installed once, before any workers start. A nil reporter retains
// the interactive interface. All JSON writes use the original stdout handle.
var events *eventReporter

type operationError struct {
	code, phase string
	reason      string
	err         error
}

func (e *operationError) Error() string { return e.err.Error() }
func (e *operationError) Unwrap() error { return e.err }
func failure(code, phase string, err error) error {
	if err == nil {
		return nil
	}
	return &operationError{code: code, phase: phase, err: err}
}

var failureMessages = map[string]string{
	"invalid_arguments":            "The downloader arguments are invalid.",
	"input_failed":                 "The downloader could not read the requested input.",
	"output_configuration_failed":  "The downloader could not configure the output location or filename.",
	"sign_in_configuration_failed": "The downloader could not configure sign-in.",
	"authentication_failed":        "The downloader could not authenticate.",
	"credential_storage_failed":    "The downloader could not access stored credentials.",
	"requested_audio_unavailable":  "A requested audio language is unavailable.",
	"network_failed":               "The downloader could not complete a network request.",
	"response_failed":              "The service returned an unusable response.",
	"body_read_failed":             "The downloader could not read a response body.",
	"download_failed":              "The downloader could not download the requested media.",
	"decryption_failed":            "The downloader could not decrypt the downloaded tracks.",
	"ffmpeg_failed":                "The downloader could not mux the requested output.",
	"output_validation_failed":     "The downloader could not verify the output file.",
	"internal_error":               "The downloader encountered an internal error.",
}

type trackIdentity struct{ id, kind, locale string }
type trackState struct {
	identity trackIdentity
	percent  int
}

type eventReporter struct {
	mu                sync.Mutex
	w                 io.Writer
	started           time.Time
	sequence          int64
	attempt           int
	phase             string
	workPhase         string // latest non-authentication phase, including parallel workers
	terminal          bool
	writeErr          error
	tracks            map[string]trackState
	warnings          []string
	cleanupApplicable bool
	outputPath        string
	skipped           bool
}

func newEventReporter(w io.Writer, started time.Time) *eventReporter {
	r := &eventReporter{w: w, started: started, phase: "initializing", workPhase: "initializing", warnings: []string{}}
	r.emitLocked("protocol", map[string]any{"tool": "crunchyroll-downloader"})
	r.emitLocked("phase", map[string]any{"phase": "initializing"})
	return r
}

// emitLocked uses one unbuffered write per line, serialized across every worker.
// On a broken pipe we retain the error and ultimately exit nonzero; successful
// delivery of a terminal record is impossible when the reader has disappeared.
func (r *eventReporter) emitLocked(event string, fields map[string]any) {
	if r.terminal || r.writeErr != nil {
		return
	}
	fields["protocol"] = "cva-downloader-progress"
	fields["version"] = 1
	fields["sequence"] = r.sequence
	fields["elapsed_ms"] = max(int64(0), time.Since(r.started).Milliseconds())
	fields["event"] = event
	line, err := json.Marshal(fields)
	if err == nil {
		line = append(line, '\n')
		var n int
		n, err = r.w.Write(line)
		if err == nil && n != len(line) {
			err = io.ErrShortWrite
		}
	}
	r.writeErr = err
	r.sequence++
}

func (r *eventReporter) startAttempt() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempt++
	r.tracks = map[string]trackState{}
	r.outputPath, r.skipped = "", false
	r.emitLocked("attempt_started", map[string]any{"attempt": r.attempt})
}

func (r *eventReporter) setPhase(phase string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if phase != r.phase {
		r.phase = phase
		if phase != "authenticating" {
			r.workPhase = phase
		}
		r.emitLocked("phase", map[string]any{"phase": phase})
	}
}

func (r *eventReporter) authStep(step, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phase = "authenticating"
	// Several steps share the same phase. Emit each operation, including a
	// second validation after a rejected saved cookie has been renewed.
	r.emitLocked("phase", map[string]any{"phase": "authenticating", "auth_step": step, "display_message": message})
}

func (r *eventReporter) resumeAfterAuthentication() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Resume the latest work phase, even when a parallel worker changed it
	// between authentication steps. Do not restore a stale pre-refresh phase.
	if r.phase == "authenticating" {
		r.phase = r.workPhase
		r.emitLocked("phase", map[string]any{"phase": r.phase})
	}
}

func trackFields(t trackIdentity, attempt int) map[string]any {
	var locale any
	if t.locale != "" {
		locale = t.locale
	}
	return map[string]any{"attempt": attempt, "track_id": t.id, "kind": t.kind, "locale": locale}
}

func (r *eventReporter) currentPhase() string {
	if r == nil {
		return "initializing"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.phase
}

func (r *eventReporter) trackStarted(t trackIdentity) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tracks[t.id]; ok {
		return
	}
	r.tracks[t.id] = trackState{identity: t}
	r.emitLocked("track_started", trackFields(t, r.attempt))
}

func (r *eventReporter) progress(t trackIdentity, current, total int64, segmented, complete bool, rate float64) {
	if r == nil || t.id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, exists := r.tracks[t.id]
	if !exists || state.identity != t {
		return
	}
	percent := 0
	if total > 0 {
		percent = int(float64(current) / float64(total) * 100)
	}
	// 100 is reserved for a successful transfer, including the final disk write.
	percent = min(99, max(state.percent, max(0, percent)))
	if complete || state.percent == 100 {
		percent = 100
	}
	state.percent = percent
	r.tracks[t.id] = state
	fields := trackFields(t, r.attempt)
	fields["percent"] = percent
	fields["segments_current"], fields["segments_total"], fields["mbps"] = nil, nil, nil
	if segmented && total > 0 {
		fields["segments_current"], fields["segments_total"] = max(int64(0), min(current, total)), total
	}
	if rate >= 0 && !math.IsNaN(rate) && !math.IsInf(rate, 0) {
		fields["mbps"] = rate
	}
	r.emitLocked("track_progress", fields)
}

func (r *eventReporter) retry(wait time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emitLocked("attempt_failed", map[string]any{"attempt": r.attempt, "code": "rate_limited", "recoverable": true, "display_message": "The download attempt was rate limited."})
	r.emitLocked("retry_wait", map[string]any{"attempt": r.attempt, "next_attempt": r.attempt + 1, "reason": "rate_limited", "delay_seconds": max(int64(1), int64(math.Ceil(wait.Seconds())))})
}

func (r *eventReporter) warning(code, message string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	found := false
	for _, warning := range r.warnings {
		if warning == code {
			found = true
		}
	}
	if !found {
		r.warnings = append(r.warnings, code)
	}
	r.emitLocked("warning", map[string]any{"code": code, "display_message": message})
}

func (r *eventReporter) streamOpened() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupApplicable = true
}

func (r *eventReporter) output(path string, skipped bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outputPath, r.skipped = path, skipped
}

func (r *eventReporter) finish(err error, format string) int {
	if r == nil {
		if err != nil {
			return 1
		}
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil && r.outputPath == "" {
		err = failure("internal_error", r.phase, fmt.Errorf("no output result"))
	}
	status := 0
	if err != nil {
		code, phase := "download_failed", r.phase
		var op *operationError
		if errors.As(err, &op) {
			code, phase = op.code, op.phase
		}
		message, ok := failureMessages[code]
		if !ok {
			code, message = "internal_error", failureMessages["internal_error"]
		}
		fields := map[string]any{"attempt": max(1, r.attempt), "phase": phase, "code": code, "display_message": message}
		if op != nil && phase == "authenticating" {
			// Only allowlisted reasons with the matching broad failure code can
			// contribute a display message. Never render err.Error() here.
			if reasonCode, detail, known := cvaFailureDetail(op.reason); known && reasonCode == code {
				fields["reason"], fields["display_message"] = op.reason, detail
			}
		}
		r.emitLocked("failed", fields)
		status = 1
	} else if r.skipped {
		r.emitLocked("skipped_existing", map[string]any{"attempt": r.attempt, "output_path": r.outputPath, "validation": "existence_only"})
	} else {
		cleanup := "not_applicable"
		if r.cleanupApplicable {
			cleanup = "confirmed"
		}
		for _, code := range r.warnings {
			if code == "cleanup_failed" {
				cleanup = "warning"
			}
		}
		r.emitLocked("completed", map[string]any{"attempt": r.attempt, "output_path": r.outputPath, "output_format": format, "cleanup_status": cleanup, "warnings": r.warnings})
	}
	r.terminal = true
	if r.writeErr != nil {
		status = 1
	}
	return status
}

// Capture the operation's phase before cleanup changes it. Already classified
// errors retain their original phase, even when another worker finishes later.
func operationFailure(err error, code, phase string) error {
	var op *operationError
	if errors.As(err, &op) {
		return err
	}
	return failure(code, phase, err)
}
