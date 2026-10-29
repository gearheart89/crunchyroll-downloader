package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseOutputFormat(t *testing.T) {
	for _, value := range []string{"mkv", "mp4", " MP4 "} {
		got, err := parseOutputFormat(value)
		if err != nil || got != strings.ToLower(strings.TrimSpace(value)) {
			t.Fatalf("parseOutputFormat(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"", "avi", ".mp4", "../mp4"} {
		if _, err := parseOutputFormat(value); err == nil {
			t.Errorf("accepted invalid format %q", value)
		}
	}
}

func TestPlexSubtitleNames(t *testing.T) {
	base := filepath.Join(t.TempDir(), "Example S01E01 - Pilot [1080p]")
	tracks := []mediaTrack{
		{locale: "en-US", format: "srt"},
		{locale: "de-DE", format: "ass", forced: true},
		{locale: "en-US", format: "vtt", isCC: true},
		{locale: "es-419", format: "ass"},
		{locale: "es-ES", format: "ass"},
		{locale: "fr-FR", format: "ssa"},
		{locale: "zh-CN", format: "ass"},
		{locale: "zh-HK", format: "ass"},
		{locale: "zh-TW", format: "ass"},
		{locale: "en-US", format: "srt", isCC: true},
		{locale: "de-DE", format: "ass"},
	}
	got, err := planSubtitleSidecars(base+".mp4", tracks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		base + ".en.srt", base + ".de.ass", base + ".en.vtt",
		base + ".es.ass", base + ".spa.ass", base + ".fr.ssa",
		base + ".zh.ass", base + ".chi.ass",
		filepath.Join(filepath.Dir(base), "subtitles", filepath.Base(base)+".zh.ass"),
		base + ".eng.srt", base + ".ger.ass",
	}
	for i, sidecar := range got {
		if sidecar.path != want[i] {
			t.Errorf("track %d = %q, want %q", i, sidecar.path, want[i])
		}
	}
	for _, track := range []mediaTrack{
		{locale: "en-US", format: "../srt"}, {locale: "en-US", format: ""}, {locale: "../en", format: "srt"},
	} {
		if _, err := planSubtitleSidecars(base+".mp4", []mediaTrack{track}); err == nil {
			t.Errorf("accepted unsafe or missing subtitle filename information: %+v", track)
		}
	}
}

func assertOutputBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file %s changed: %q, error %v; want %q", path, got, err, want)
	}
}

func TestCopySubtitleSidecarPreservesBytesAndExistingFiles(t *testing.T) {
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "source.srt"), filepath.Join(dir, "Episode.en.srt")
	data := []byte("\xef\xbb\xbf1\r\n00:00:00,100 --> 00:00:00,900\r\nHéllo 世界\r\n")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := copySubtitleSidecar(source, destination); err != nil || !created {
		t.Fatalf("first copy = %v, %v", created, err)
	}
	assertOutputBytes(t, destination, data)
	if created, err := copySubtitleSidecar(source, destination); err != nil || created {
		t.Fatalf("identical retry = %v, %v", created, err)
	}
	if err := os.WriteFile(source, []byte("different subtitle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := copySubtitleSidecar(source, destination); err == nil {
		t.Fatal("overwrote an existing subtitle")
	}
	assertOutputBytes(t, destination, data)
}

func runMediaTool(t *testing.T, tool string, args ...string) []byte {
	t.Helper()
	data, err := exec.Command(tool, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", tool, err, data)
	}
	return data
}

func requireMediaTools(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg is required for the synthetic output integration test")
	}
	ffprobe, err = exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("FFprobe is required for the synthetic output integration test")
	}
	previous := ffmpegPath
	ffmpegPath = ffmpeg
	t.Cleanup(func() { ffmpegPath = previous })
	return ffmpeg, ffprobe
}

// Fixtures are generated locally; no credentials or network are needed.
func makeOutputInputs(t *testing.T, ffmpeg, dir string) (string, []mediaTrack, []mediaTrack) {
	t.Helper()
	video := filepath.Join(dir, "video.mp4")
	runMediaTool(t, ffmpeg, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=c=black:s=64x64:r=10:d=1", "-c:v", "mpeg4", "-an", video)
	var audio []mediaTrack
	for _, locale := range []string{"ja-JP", "en-US"} {
		path := filepath.Join(dir, locale+".m4a")
		runMediaTool(t, ffmpeg, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "aac", path)
		audio = append(audio, mediaTrack{file: path, locale: locale})
	}
	sources := []struct {
		track mediaTrack
		body  string
	}{
		{mediaTrack{locale: "de-DE", format: "ass", forced: true}, "[Script Info]\nScriptType: v4.00+\nPlayResX: 640\nPlayResY: 480\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\nStyle: Default,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:00.10,0:00:00.90,Default,,0,0,0,,{\\pos(20,30)\\i1}Schild\n"},
		{mediaTrack{locale: "en-US", format: "vtt", isCC: true}, "WEBVTT\n\n00:00.100 --> 00:00.900 line:90% align:center\n[phone ringing]\n"},
		{mediaTrack{locale: "en-US", format: "srt"}, "1\r\n00:00:00,100 --> 00:00:00,900\r\nHello\r\n"},
	}
	var subtitles []mediaTrack
	for _, source := range sources {
		track := source.track
		track.file = filepath.Join(dir, "source."+track.format)
		if err := os.WriteFile(track.file, []byte(source.body), 0o600); err != nil {
			t.Fatal(err)
		}
		subtitles = append(subtitles, track)
	}
	return video, audio, subtitles
}

type outputProbe struct {
	Streams []struct {
		CodecType   string `json:"codec_type"`
		CodecName   string `json:"codec_name"`
		Tags        map[string]string
		Disposition struct{ Default, Forced int }
	}
}

func packetHashes(t *testing.T, ffprobe, file, stream string) []string {
	t.Helper()
	var result struct {
		Packets []struct {
			Hash string `json:"data_hash"`
		}
	}
	data := runMediaTool(t, ffprobe, "-v", "error", "-select_streams", stream, "-show_packets", "-show_data_hash", "sha256", "-show_entries", "packet=data_hash", "-of", "json", file)
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	var hashes []string
	for _, packet := range result.Packets {
		hashes = append(hashes, packet.Hash)
	}
	if len(hashes) == 0 {
		t.Fatal("no encoded packets in fixture")
	}
	return hashes
}

func TestOutputContainersWithFFmpeg(t *testing.T) {
	ffmpeg, ffprobe := requireMediaTools(t)
	for _, format := range []string{"mp4", "mkv"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			work := filepath.Join(dir, "work")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(work)
			video, audio, subtitles := makeOutputInputs(t, ffmpeg, work)
			videoHashes := packetHashes(t, ffprobe, video, "v:0")
			audioHashes := packetHashes(t, ffprobe, audio[0].file, "a:0")
			var originalSubs [][]byte
			for _, sub := range subtitles {
				data, err := os.ReadFile(sub.file)
				if err != nil {
					t.Fatal(err)
				}
				originalSubs = append(originalSubs, data)
			}
			destination, filename, err := prepareOutputOptions("../Media/Season 01", "Custom S01E01 - Pilot [1080p]", format,
				[]string{"https://www.crunchyroll.com/watch/TEST/episode"})
			if err != nil {
				t.Fatal(err)
			}
			output, err := episodeOutputPath(EpisodeInfo{}, "1080p", destination, filename, format)
			if err != nil {
				t.Fatal(err)
			}
			mergeEverything(video, audio, subtitles, output, EpisodeInfo{})
			var probe outputProbe
			if err := json.Unmarshal(runMediaTool(t, ffprobe, "-v", "error", "-show_streams", "-of", "json", output), &probe); err != nil {
				t.Fatal(err)
			}
			wantCount := 3
			if format == "mkv" {
				wantCount += 3
			}
			if len(probe.Streams) != wantCount {
				t.Fatalf("got %d streams, want %d: %+v", len(probe.Streams), wantCount, probe.Streams)
			}
			for i, language := range []string{"jpn", "eng"} {
				stream := probe.Streams[i+1]
				if stream.CodecType != "audio" || stream.CodecName != "aac" || stream.Tags["language"] != language || stream.Disposition.Default != 1-i {
					t.Errorf("unexpected audio track %d: %+v", i, stream)
				}
			}
			if !reflect.DeepEqual(packetHashes(t, ffprobe, output, "v:0"), videoHashes) || !reflect.DeepEqual(packetHashes(t, ffprobe, output, "a:0"), audioHashes) {
				t.Fatal("video or audio encoded payload changed during muxing")
			}
			if format == "mp4" {
				sidecars, err := planSubtitleSidecars(output, subtitles)
				if err != nil {
					t.Fatal(err)
				}
				for i, sidecar := range sidecars {
					assertOutputBytes(t, sidecar.path, originalSubs[i])
				}
			} else {
				for i, codec := range []string{"ass", "subrip", "subrip"} {
					if probe.Streams[3+i].CodecName != codec {
						t.Errorf("MKV subtitle %d = %s, want %s", i, probe.Streams[3+i].CodecName, codec)
					}
				}
				if probe.Streams[3].Disposition.Forced != 1 {
					t.Error("MKV forced disposition missing")
				}
				if _, err := os.Stat(strings.TrimSuffix(output, ".mkv") + ".en.srt"); !os.IsNotExist(err) {
					t.Error("MKV unexpectedly exported a sidecar")
				}
			}
			for _, path := range []string{video, audio[0].file, audio[1].file, subtitles[0].file, subtitles[1].file, subtitles[2].file} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("temporary file was not cleaned up: %s", path)
				}
			}
		})
	}
}

func TestFailedOutputPreservesInputsAndExistingSidecars(t *testing.T) {
	ffmpeg, _ := requireMediaTools(t)
	dir := t.TempDir()
	video, audio, subtitles := makeOutputInputs(t, ffmpeg, dir)
	output := filepath.Join(dir, "Episode.mp4")
	protected := filepath.Join(dir, "Episode.en.vtt")
	if err := os.WriteFile(protected, []byte("user's existing subtitle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeOutput(video, audio, subtitles, output, EpisodeInfo{}); err == nil {
		t.Fatal("expected conflicting sidecar error")
	}
	assertOutputBytes(t, protected, []byte("user's existing subtitle"))
	for _, path := range []string{output, filepath.Join(dir, "Episode.de.ass")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("partial output left behind: %s", path)
		}
	}
	for _, path := range []string{video, audio[0].file, subtitles[0].file} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("input removed on failure: %s", path)
		}
	}
	if err := os.WriteFile(video, []byte("invalid video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeOutput(video, audio, nil, output, EpisodeInfo{}); err == nil {
		t.Fatal("expected FFmpeg failure")
	}
	assertOutputBytes(t, video, []byte("invalid video"))
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Error("failed mux left a final MP4")
	}
	staged, err := filepath.Glob(filepath.Join(dir, ".crdl-output-*"))
	if err != nil || len(staged) != 0 {
		t.Fatalf("staged output not cleaned up: %v, %v", staged, err)
	}
}

func TestMP4WithoutSubtitles(t *testing.T) {
	ffmpeg, ffprobe := requireMediaTools(t)
	dir := t.TempDir()
	video, audio, _ := makeOutputInputs(t, ffmpeg, dir)
	output := filepath.Join(dir, "Episode.mp4")
	if err := writeOutput(video, audio, nil, output, EpisodeInfo{}); err != nil {
		t.Fatal(err)
	}
	var probe outputProbe
	if err := json.Unmarshal(runMediaTool(t, ffprobe, "-v", "error", "-show_streams", "-of", "json", output), &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Streams) != 3 {
		t.Fatalf("expected video and two audio streams: %+v", probe.Streams)
	}
	sidecars, err := filepath.Glob(filepath.Join(dir, "Episode.*.*"))
	if err != nil || len(sidecars) != 0 {
		t.Fatalf("unexpected subtitles: %v, %v", sidecars, err)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeOutput(video, audio, nil, output, EpisodeInfo{}); err == nil {
		t.Fatal("existing output was not rejected")
	}
	assertOutputBytes(t, output, before)
}
