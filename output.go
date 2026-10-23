package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// mediaTrack pairs a downloaded temporary file with the locale it represents.
type mediaTrack struct {
	file   string
	locale string
	format string // subtitle source format, e.g. "ass" or "vtt". Unused for audio tracks.
	isCC   bool   // true if this track is a closed caption rather than a regular subtitle
	forced bool   // explicitly identified as forced, never inferred from the default track
}

func parseOutputFormat(value string) (string, error) {
	format := strings.ToLower(strings.TrimSpace(value))
	if format != "mkv" && format != "mp4" {
		return "", fmt.Errorf("invalid --output-format %q: choose mkv or mp4", value)
	}
	return format, nil
}

// trackTitle returns a human-readable track name for a locale, falling back to
// the raw locale when it isn't in the known list.
func trackTitle(locale string) string {
	if name, ok := languageNames[locale]; ok {
		return name
	}
	return locale
}

// muxArgs keeps MKV subtitles embedded and leaves MP4 subtitles for sidecar export.
func muxArgs(videoFile string, audioTracks, subTracks []mediaTrack, outputFile string, info EpisodeInfo) []string {
	mp4 := strings.EqualFold(filepath.Ext(outputFile), ".mp4")
	if mp4 {
		subTracks = nil
	}
	args := []string{"-nostdin", "-y", "-i", videoFile}
	for _, audio := range audioTracks {
		args = append(args, "-i", audio.file)
	}
	for _, sub := range subTracks {
		args = append(args, "-i", sub.file)
	}

	// Map every input stream explicitly; without this ffmpeg keeps only one
	// stream of each type.
	args = append(args, "-map", "0:v:0")
	for i := range audioTracks {
		args = append(args, "-map", fmt.Sprintf("%d:a:0", 1+i))
	}
	for j := range subTracks {
		args = append(args, "-map", fmt.Sprintf("%d", 1+len(audioTracks)+j))
	}

	args = append(args, "-c:v", "copy", "-c:a", "copy")
	// WebVTT cue-positioning metadata (e.g. "line:90% align:center") isn't
	// rendered correctly by most players when copied as-is into an MKV, so
	// VTT subtitle/caption tracks are transcoded to SRT (which has no
	// positioning syntax) instead of stream-copied. ASS tracks keep "copy"
	// to preserve their styling.
	for j, sub := range subTracks {
		if sub.format == "vtt" {
			args = append(args, fmt.Sprintf("-c:s:%d", j), "srt")
		} else {
			args = append(args, fmt.Sprintf("-c:s:%d", j), "copy")
		}
	}

	for i, audio := range audioTracks {
		args = append(args,
			fmt.Sprintf("-metadata:s:a:%d", i), "language="+languageCodes[audio.locale],
			fmt.Sprintf("-metadata:s:a:%d", i), "title="+trackTitle(audio.locale),
		)
	}
	for j, sub := range subTracks {
		title := trackTitle(sub.locale)
		if sub.isCC {
			title += " [CC]"
		}
		if sub.forced {
			title += " [Forced]"
		}
		args = append(args,
			fmt.Sprintf("-metadata:s:s:%d", j), "language="+languageCodes[sub.locale],
			fmt.Sprintf("-metadata:s:s:%d", j), "title="+title,
		)
	}
	// Mark only the first audio track and the first non-CC subtitle track as
	// default. Disposition must be set on every track: each downloaded audio
	// file is a standalone default stream, so the non-primary ones must be
	// explicitly cleared. Closed captions are never marked default; they're
	// an opt-in extra a viewer toggles deliberately.
	for i := range audioTracks {
		disposition := "0"
		if i == 0 {
			disposition = "default"
		}
		args = append(args, fmt.Sprintf("-disposition:a:%d", i), disposition)
	}
	defaultSubSet := false
	for j, sub := range subTracks {
		disposition := "0"
		if !sub.isCC && !defaultSubSet {
			disposition = "default"
			defaultSubSet = true
		}
		if sub.forced {
			if disposition == "0" {
				disposition = "forced"
			} else {
				disposition += "+forced"
			}
		}
		args = append(args, fmt.Sprintf("-disposition:s:%d", j), disposition)
	}

	args = append(args,
		"-metadata:g", "title="+fmt.Sprintf("S%02vE%02v - %s", info.EpisodeMetadata.SeasonNumber, info.EpisodeMetadata.EpisodeNumber, info.Title),
		"-metadata:g", "show="+info.EpisodeMetadata.SeriesTitle,
		"-metadata:g", "track="+fmt.Sprintf("%v", info.EpisodeMetadata.EpisodeNumber),
		"-metadata:g", "season_number="+fmt.Sprintf("%v", info.EpisodeMetadata.EpisodeNumber),
	)
	if mp4 {
		args = append(args, "-movflags", "+faststart")
	}
	return append(args, outputFile)
}

// mergeEverything only cleans up downloaded inputs after every requested output
// is complete. The MP4 itself contains video and audio; subtitles remain intact.
func mergeEverything(videoFile string, audioTracks, subTracks []mediaTrack, outputFile string, info EpisodeInfo) {
	if err := writeOutput(videoFile, audioTracks, subTracks, outputFile, info); err != nil {
		panic(err)
	}

	_ = os.Remove(videoFile)
	for _, audio := range audioTracks {
		_ = os.Remove(audio.file)
	}
	for _, sub := range subTracks {
		_ = os.Remove(sub.file)
	}
	fmt.Printf("\nDownload finished! Output file: %s\n\n", outputFile)
}

func writeOutput(videoFile string, audioTracks, subTracks []mediaTrack, outputFile string, info EpisodeInfo) (err error) {
	if _, err := os.Lstat(outputFile); err == nil {
		return fmt.Errorf("output already exists: %s", outputFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Stage on the destination filesystem so a failure never leaves a partial
	// video that the next run would mistake for a finished episode.
	stage, err := os.MkdirTemp(filepath.Dir(outputFile), ".crdl-output-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stagedVideo := filepath.Join(stage, filepath.Base(outputFile))
	var sidecars []subtitleSidecar
	if strings.EqualFold(filepath.Ext(outputFile), ".mp4") {
		sidecars, err = planSubtitleSidecars(outputFile, subTracks)
		if err != nil {
			return err
		}
	}

	cmd := exec.Command(ffmpegPath, muxArgs(videoFile, audioTracks, subTracks, stagedVideo, info)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg failed: %w\n%s", err, stderr.String())
	}

	// Roll back only the sidecars created by this attempt. Never overwrite an
	// existing subtitle; an identical one can be reused after an interrupted run.
	var created []string
	defer func() {
		if err != nil {
			for _, path := range created {
				_ = os.Remove(path)
			}
		}
	}()
	for _, sidecar := range sidecars {
		wrote, copyErr := copySubtitleSidecar(sidecar.source, sidecar.path)
		if copyErr != nil {
			return copyErr
		}
		if wrote {
			created = append(created, sidecar.path)
		}
	}
	if _, err := os.Lstat(outputFile); err == nil {
		return fmt.Errorf("output already exists: %s", outputFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stagedVideo, outputFile); err != nil {
		return err
	}
	for _, sidecar := range sidecars {
		fmt.Printf("Subtitle %s: %s\n", sidecar.locale, sidecar.path)
	}
	return nil
}

type subtitleSidecar struct {
	source string
	path   string
	locale string
}

// Plex accepts ISO 639-1 and ISO 639-2/B, not the regional Crunchyroll locale.
// A second regional variant can use the three-letter alias without colliding.
func plexLanguageCodes(locale string) ([]string, error) {
	parts := strings.Split(strings.ToLower(locale), "-")
	if len(parts[0]) != 2 || strings.Trim(parts[0], "abcdefghijklmnopqrstuvwxyz") != "" {
		return nil, fmt.Errorf("cannot name Plex subtitle for language %q", locale)
	}
	codes := []string{parts[0]}
	if code := languageCodes[locale]; code != "" {
		if bibliographic, ok := map[string]string{"deu": "ger", "fra": "fre", "msa": "may", "zho": "chi"}[code]; ok {
			code = bibliographic
		}
		codes = append(codes, code)
	}
	return codes, nil
}

func planSubtitleSidecars(outputFile string, tracks []mediaTrack) ([]subtitleSidecar, error) {
	base := strings.TrimSuffix(outputFile, filepath.Ext(outputFile))
	used := make(map[string]bool)
	var sidecars []subtitleSidecar
	for _, track := range tracks {
		ext := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(track.format), "."))
		if ext == "" || strings.Trim(ext, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
			return nil, fmt.Errorf("invalid source subtitle format %q for %s", track.format, track.locale)
		}
		codes, err := plexLanguageCodes(track.locale)
		if err != nil {
			return nil, err
		}
		flags := ""
		if track.forced {
			flags += ".forced"
		}
		if track.isCC {
			flags += ".cc"
		}
		var path string
		// If both language aliases are already occupied, Plex also supports
		// a sibling "subtitles" directory (Plex Media Server 1.41.0+).
		for _, stem := range []string{base, filepath.Join(filepath.Dir(base), "subtitles", filepath.Base(base))} {
			for _, code := range codes {
				candidate := stem + "." + code + flags + "." + ext
				if !used[strings.ToLower(candidate)] {
					path = candidate
					break
				}
			}
			if path != "" {
				break
			}
		}
		if path == "" {
			return nil, fmt.Errorf("Plex subtitle filename collision for %s: select fewer regional variants of this language/format", track.locale)
		}
		used[strings.ToLower(path)] = true
		sidecars = append(sidecars, subtitleSidecar{source: track.file, path: path, locale: track.locale})
	}
	return sidecars, nil
}

func copySubtitleSidecar(source, destination string) (created bool, err error) {
	data, err := os.ReadFile(source)
	if err != nil {
		return false, fmt.Errorf("reading subtitle: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return false, fmt.Errorf("creating subtitle directory: %w", err)
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := os.ReadFile(destination)
		if readErr == nil && bytes.Equal(existing, data) {
			return false, nil
		}
		return false, fmt.Errorf("refusing to replace existing subtitle %s", destination)
	}
	if err != nil {
		return false, fmt.Errorf("creating subtitle: %w", err)
	}
	_, writeErr := io.Copy(out, bytes.NewReader(data))
	closeErr := out.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(destination)
		return false, fmt.Errorf("writing subtitle: %w", err)
	}
	return true, nil
}
