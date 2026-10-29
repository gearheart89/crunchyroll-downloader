package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeOutputFilename(t *testing.T) {
	for _, tc := range []struct{ name, format, want string }{
		{"", "mkv", ""},
		{"Show S01E02 - Pilot", "mp4", "Show S01E02 - Pilot.mp4"},
		{"Show.S01E02.Pilot", "mp4", "Show.S01E02.Pilot.mp4"},
		{"Show S01E02.mp4", "mp4", "Show S01E02.mp4"},
		{"Show S01E02.MP4", "mp4", "Show S01E02.MP4"},
		{"L'été 日本語 [1080p]", "mkv", "L'été 日本語 [1080p].mkv"},
	} {
		got, err := normalizeOutputFilename(tc.name, tc.format)
		if err != nil || got != tc.want {
			t.Errorf("normalizeOutputFilename(%q, %q) = %q, %v; want %q", tc.name, tc.format, got, err, tc.want)
		}
	}
	invalid := []string{".", "..", "../Episode", `..\Episode`, "/Episode", `C:\Episode`,
		"dir/Episode", `dir\Episode`, " ", "Episode.", "Episode ", "Episode:Part 1", "Episode?", "Episode\x00", "Episode\n", ".mp4", "Episode.mkv"}
	if runtime.GOOS == "windows" {
		invalid = append(invalid, "CON", "NUL.mp4", "COM1")
	}
	for _, name := range invalid {
		if _, err := normalizeOutputFilename(name, "mp4"); err == nil {
			t.Errorf("accepted invalid filename %q", name)
		}
	}
}

func TestEpisodeOutputPath(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "jobs", "worker")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	info := EpisodeInfo{Title: "A Pilot", EpisodeMetadata: EpisodeMetadata{SeriesTitle: "My Show", SeasonNumber: 2, EpisodeNumber: 3}}
	defaultName := "My Show S02E03 - A Pilot [1080p].mp4"
	for _, tc := range []struct{ name, directory, filename, want string }{
		{"defaults", "", "", filepath.Join(work, "My Show", defaultName)},
		{"directory only", "exports", "", filepath.Join(work, "exports", defaultName)},
		{"filename only", "", "Custom", filepath.Join(work, "My Show", "Custom.mp4")},
		{"current directory", ".", "Custom.mp4", filepath.Join(work, "Custom.mp4")},
		{"parent", "../exports/Season 02", "Custom", filepath.Join(root, "jobs", "exports", "Season 02", "Custom.mp4")},
		{"two parents", "../../exports/./日本語", "Custom", filepath.Join(root, "exports", "日本語", "Custom.mp4")},
		{"absolute", filepath.Join(root, "Library", "Season 02"), "Custom.mp4", filepath.Join(root, "Library", "Season 02", "Custom.mp4")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := episodeOutputPath(info, "1080p", tc.directory, tc.filename, "mp4")
			if err != nil || got != tc.want {
				t.Fatalf("output path = %q, %v; want %q", got, err, tc.want)
			}
			sidecars, err := planSubtitleSidecars(got, []mediaTrack{{locale: "en-US", format: "srt", forced: true}})
			if err != nil || len(sidecars) != 1 || sidecars[0].path != strings.TrimSuffix(tc.want, ".mp4")+".en.srt" {
				t.Fatalf("subtitle did not follow custom video name: %+v, %v", sidecars, err)
			}
		})
	}
	if runtime.GOOS == "windows" {
		for _, tc := range []struct{ directory, want string }{
			{`..\..\exports\Season 02`, filepath.Join(root, "exports", "Season 02", "Custom.mp4")},
			{`C:\Media\Jobs\..\Shows`, `C:\Media\Shows\Custom.mp4`},
			{`\\server\share\Jobs\..\Shows`, `\\server\share\Shows\Custom.mp4`},
		} {
			got, err := episodeOutputPath(info, "1080p", tc.directory, "Custom", "mp4")
			if err != nil || got != tc.want {
				t.Errorf("Windows path %q = %q, %v; want %q", tc.directory, got, err, tc.want)
			}
		}
	} else {
		got, err := episodeOutputPath(info, "1080p", "/media/jobs/../Shows", "Custom", "mp4")
		if err != nil || got != "/media/Shows/Custom.mp4" {
			t.Fatalf("Linux mount path = %q, %v", got, err)
		}
	}
}

func TestPrepareOutputOptions(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "jobs", "worker")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	watch := []string{"https://www.crunchyroll.com/watch/TEST/episode"}
	dir, name, err := prepareOutputOptions("../../Media/Show/Season 01", "Custom", "mp4", watch)
	want := filepath.Join(root, "Media", "Show", "Season 01")
	if err != nil || dir != want || name != "Custom.mp4" {
		t.Fatalf("prepareOutputOptions = %q, %q, %v", dir, name, err)
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("output directory was not created: %v", err)
	}
	if cwd, err := os.Getwd(); err != nil || cwd != work {
		t.Fatalf("working directory changed to %q: %v", cwd, err)
	}
	series := []string{"https://www.crunchyroll.com/series/TEST/show"}
	many := append(append([]string{}, watch...), watch...)
	for _, urls := range [][]string{nil, series, many, {"https://www.crunchyroll.com/watch"}} {
		if _, _, err := prepareOutputOptions("must-not-exist", "Custom", "mp4", urls); err == nil {
			t.Errorf("accepted ambiguous target for a literal filename: %v", urls)
		}
	}
	if _, _, err := prepareOutputOptions("must-not-exist", "Custom.mkv", "mp4", watch); err == nil {
		t.Error("accepted container mismatch")
	}
	if _, err := os.Stat("must-not-exist"); !os.IsNotExist(err) {
		t.Error("created output directory before validating input")
	}
	for _, urls := range [][]string{series, many} {
		if _, name, err := prepareOutputOptions(want, "", "mp4", urls); err != nil || name != "" {
			t.Errorf("directory-only option rejected a batch: %q, %v", name, err)
		}
	}
	blocker := filepath.Join(root, "file-not-directory")
	if err := os.WriteFile(blocker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareOutputOptions(filepath.Join(blocker, "child"), "Custom", "mp4", watch); err == nil {
		t.Error("ignored directory creation failure")
	}
	assertOutputBytes(t, blocker, []byte("unchanged"))
}
