package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	runtimedebug "runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// Qualified by synthetic media and transport acceptance on Windows and Linux.
// Live provider/CVA consumer acceptance is a separate integration check.
const mediaWindowsQualified = true

func windowModeRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		n := strings.SplitN(strings.TrimLeft(a, "-"), "=", 2)[0]
		if n == "request-json" || n == "capabilities-json" {
			return true
		}
	}
	return false
}
func downloaderBuild() string {
	if b, ok := runtimedebug.ReadBuildInfo(); ok {
		rev := "source"
		for _, s := range b.Settings {
			if s.Key == "vcs.revision" {
				rev = s.Value
			}
			if s.Key == "vcs.modified" && s.Value == "true" {
				rev += "-modified"
			}
		}
		return "cva-media-windows-v1-" + rev
	}
	return "cva-media-windows-v1-source"
}
func windowCapabilities() map[string]any {
	ops := []any{}
	if mediaWindowsQualified && windowPlatformSupported() {
		ops = append(ops, map[string]any{"name": "acquire_windows", "capability": "media_windows_v1", "request_versions": []int{1}, "result_versions": []int{1}, "sample_map_versions": []int{1}, "progress_versions": []int{1}, "delivery_formats": []string{"mp4"}, "video_only": true, "codec_configuration_changes": "reject", "boundary_policies": []string{"clamp_to_episode", "require_requested_interval"}})
	}
	return map[string]any{"schema": "cva.downloader-capabilities", "version": 1, "downloader_version": downloaderBuild(), "operations": ops}
}
func windowPlatformSupported() bool {
	return runtime.GOOS == "windows" || (runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"))
}

type windowReporter struct {
	out      io.Writer
	sequence int
	request  any
	attempt  string
	err      error
}

func (r *windowReporter) emit(v map[string]any) {
	if r.err != nil {
		return
	}
	r.sequence++
	v["protocol"] = "cva-media-window-progress"
	v["version"] = 1
	v["sequence"] = r.sequence
	v["request_id"] = r.request
	v["attempt_id"] = r.attempt
	b := windowJSON(v)
	if len(b) > 65536 || validateWindowJSON("progress", b) != nil {
		r.err = wf("internal_error")
		return
	}
	_, r.err = r.out.Write(b)
}
func (r *windowReporter) phase(s string) { r.emit(map[string]any{"event": "phase", "phase": s}) }
func (r *windowReporter) terminal(status string, result any, code any) {
	r.emit(map[string]any{"event": "terminal", "status": status, "result": result, "error_code": code, "message": map[string]string{"completed": "Requested media windows acquired.", "partial": "Only partial requested coverage was acquired.", "failed": "Media-window acquisition failed."}[status]})
}
func windowHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func windowFileHash(path string) (string, int64, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, e
}

type windowRunner func(context.Context, windowRequest, string, *windowReporter, map[string]any, cvaAuthOptions) error

func runWindowCLI(args []string, out io.Writer) int {
	return runWindowCLIWith(args, out, runWindowAcquisition)
}

func runWindowAcquisition(ctx context.Context, request windowRequest, stage string, r *windowReporter, result map[string]any, options cvaAuthOptions) error {
	r.phase("authenticating")
	var e error
	accessTokenProvider, e = newAccessTokenProvider("", options)
	if e != nil {
		return wf("authentication_failed")
	}
	tok, e := accessTokenProvider()
	if e != nil {
		return wf("authentication_failed")
	}
	tokenMu.Lock()
	token = tok
	tokenMu.Unlock()
	return acquireMediaWindows(ctx, request, stage, r, result)
}

func runWindowCLIWith(args []string, out io.Writer, run windowRunner) (exit int) {
	r := &windowReporter{out: out, attempt: uuid.NewString()}
	var terminal bool
	defer func() {
		if recover() != nil && !terminal {
			r.terminal("failed", nil, "internal_error")
			exit = 1
		}
	}()
	fs := flag.NewFlagSet("media-windows", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	requestPath := fs.String("request-json", "", "")
	caps := fs.Bool("capabilities-json", false, "")
	root := fs.String("cva-root", "", "")
	python := fs.String("cva-python", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 0 || (*caps && (*requestPath != "" || fs.NFlag() != 1)) || (!*caps && *requestPath == "") {
		r.terminal("failed", nil, "invalid_request")
		terminal = true
		return 1
	}
	if *caps {
		_, e := out.Write(windowJSON(windowCapabilities()))
		if e != nil {
			return 1
		}
		return 0
	}
	f, e := os.Open(*requestPath)
	if e != nil {
		r.terminal("failed", nil, "invalid_request")
		terminal = true
		return 1
	}
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	f.Close()
	if e != nil {
		r.terminal("failed", nil, "invalid_request")
		terminal = true
		return 1
	}
	request, e := parseWindowRequest(b)
	if e != nil {
		r.terminal("failed", nil, "invalid_request")
		terminal = true
		return 1
	}
	r.request = request.RequestID
	if !windowPlatformSupported() {
		r.terminal("failed", nil, "unsupported_capability")
		terminal = true
		return 1
	}
	r.phase("initializing")
	stage, e := beginWindowStage(request.Output.Directory, r.attempt)
	if e != nil {
		r.terminal("failed", nil, "output_configuration_failed")
		terminal = true
		return 1
	}
	defer stage.close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	// Existing provider diagnostics must never enter the new protocol or disclose
	// raw service errors. Auth progress on stderr already uses fixed safe text.
	null, e := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if e != nil {
		r.terminal("failed", nil, "internal_error")
		terminal = true
		return 1
	}
	oldOut := os.Stdout
	os.Stdout = null
	defer func() { os.Stdout = oldOut; null.Close() }()
	result := newWindowResult(request, r.attempt)
	func() {
		defer func() {
			if recover() != nil {
				e = wf("internal_error")
			}
		}()
		e = run(ctx, request, stage.path, r, result, cvaAuthOptions{Root: *root, Python: *python, SourceURL: "https://www.crunchyroll.com/watch/" + request.Source.WatchID})
	}()
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			e = wf("timeout")
		} else {
			e = wf("cancelled")
		}
		// Cancellation is not permission to publish completed-looking artifacts.
		// A failed result carries no acceptable clips, even if validation finished
		// just before the cancellation arrived.
		for _, item := range result["clips"].([]any) {
			clip := item.(map[string]any)
			os.Remove(filepath.Join(stage.path, clip["path"].(string)))
			os.Remove(filepath.Join(stage.path, clip["timeline"].(map[string]any)["path"].(string)))
		}
		result["clips"] = []any{}
		for _, item := range result["windows"].([]any) {
			w := item.(map[string]any)
			w["status"] = "failed"
			w["clip_ids"] = []string{}
		}
	}
	if e != nil {
		result["errors"] = append(result["errors"].([]any), map[string]any{"code": windowErrorCode(e), "window_id": nil, "message": "The requested acquisition could not be completed."})
	}
	finalizeWindowResult(result)
	if e = stage.removeWork(); e != nil {
		r.terminal("failed", nil, "output_configuration_failed")
		terminal = true
		return 1
	}
	r.phase("publishing")
	data := windowJSON(result)
	if validateWindowJSON("result", data) != nil {
		r.terminal("failed", nil, "output_validation_failed")
		terminal = true
		return 1
	}
	if r.err != nil {
		return 1
	}
	if e = stage.publish(data); e != nil {
		r.terminal("failed", nil, "output_configuration_failed")
		terminal = true
		return 1
	}
	status := result["status"].(string)
	var code any
	if status != "completed" {
		code = result["errors"].([]any)[0].(map[string]any)["code"]
	}
	r.terminal(status, map[string]any{"path": "result.json", "sha256": windowHash(data)}, code)
	terminal = true
	if status != "completed" || r.err != nil {
		return 1
	}
	return 0
}

type windowStage struct {
	path, destination, lock string
	published               bool
}

func checkWindowParents(path string) error {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		fi, e := os.Lstat(p)
		if e != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return wf("output_configuration_failed")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func beginWindowStage(destination, attempt string) (*windowStage, error) {
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return nil, wf("output_configuration_failed")
	}
	parent := filepath.Dir(destination)
	if parent == destination || checkWindowParents(parent) != nil {
		return nil, wf("output_configuration_failed")
	}
	if _, e := os.Lstat(destination); !errors.Is(e, os.ErrNotExist) {
		return nil, wf("output_configuration_failed")
	}
	lock := filepath.Join(parent, ".cva-acquire-"+windowHash([]byte(strings.ToLower(destination)))+".lock")
	f, e := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return nil, wf("output_configuration_failed")
	}
	f.Close()
	path, e := os.MkdirTemp(parent, ".cva-"+attempt+"-")
	if e != nil {
		os.Remove(lock)
		return nil, e
	}
	s := &windowStage{path: path, destination: destination, lock: lock}
	if e = os.Mkdir(filepath.Join(path, "work"), 0700); e != nil {
		s.close()
		return nil, e
	}
	return s, nil
}
func (s *windowStage) removeWork() error { return os.RemoveAll(filepath.Join(s.path, "work")) }
func (s *windowStage) close() {
	if !s.published {
		os.RemoveAll(s.path)
	}
	os.Remove(s.lock)
}
func (s *windowStage) publish(data []byte) error {
	if checkWindowParents(filepath.Dir(s.destination)) != nil {
		return wf("output_configuration_failed")
	}
	if _, e := os.Lstat(s.destination); !errors.Is(e, os.ErrNotExist) {
		return wf("output_configuration_failed")
	}
	f, e := os.OpenFile(filepath.Join(s.path, "result.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = publishWindowDirectory(s.path, s.destination); e != nil {
		return e
	}
	s.published = true
	return nil
}
func newWindowResult(req windowRequest, attempt string) map[string]any {
	return map[string]any{"schema": "cva.media-window-result", "version": 1, "request_id": req.RequestID, "attempt_id": attempt, "status": "failed", "acquired_at": time.Now().UTC().Format(time.RFC3339), "downloader_version": downloaderBuild(), "source": nil, "representation": nil, "windows": []any{}, "clips": []any{}, "errors": []any{}}
}
func finalizeWindowResult(result map[string]any) {
	clips := result["clips"].([]any)
	errs := result["errors"].([]any)
	if len(clips) == 0 {
		result["status"] = "failed"
		if len(errs) == 0 {
			result["errors"] = []any{map[string]any{"code": "coverage_incomplete", "window_id": nil, "message": "No independently usable video coverage was acquired."}}
		}
		return
	}
	if len(errs) > 0 {
		result["status"] = "partial"
		return
	}
	result["status"] = "completed"
}
