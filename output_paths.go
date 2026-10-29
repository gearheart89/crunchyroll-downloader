package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// normalizeOutputFilename preserves a caller's literal basename. Directory
// traversal belongs in --output-dir, so filename errors are reported rather
// than silently sanitizing a name chosen by another program.
func normalizeOutputFilename(name, format string) (string, error) {
	if name == "" {
		return "", nil
	}
	if !filepath.IsLocal(name) || strings.ContainsAny(name, `/\:*?"<>|`) ||
		strings.TrimSpace(name) == "" || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "", fmt.Errorf("invalid --filename %q: use a filename only; put directories and .. in --output-dir", name)
	}
	for _, char := range name {
		if char < 32 || char == 127 {
			return "", fmt.Errorf("invalid --filename: control characters are not allowed")
		}
	}

	ext := filepath.Ext(name)
	if strings.EqualFold(ext, ".mkv") || strings.EqualFold(ext, ".mp4") {
		if !strings.EqualFold(ext, "."+format) {
			return "", fmt.Errorf("--filename extension %q does not match --output-format %s", ext, format)
		}
		if strings.TrimSpace(strings.TrimSuffix(name, ext)) == "" {
			return "", fmt.Errorf("--filename must include a name before the container extension")
		}
		return name, nil
	}
	return name + "." + format, nil
}

// prepareOutputOptions runs before authentication or downloads. A literal name
// is per episode, while an output directory can also be shared by a batch.
func prepareOutputOptions(directory, filename, format string, urls []string) (string, string, error) {
	name, err := normalizeOutputFilename(filename, format)
	if err != nil {
		return "", "", err
	}
	if name != "" {
		if len(urls) != 1 {
			return "", "", fmt.Errorf("--filename requires a single episode URL; invoke once per episode when supplying filenames")
		}
		kind, id := parseUrl(urls[0])
		if kind != "watch" || id == "" {
			return "", "", fmt.Errorf("--filename requires a single /watch/ episode URL, not a series URL")
		}
	}
	if directory != "" {
		// Abs uses native path rules, including .., Windows drives/UNC paths,
		// and Linux container mount paths. Never change the process directory:
		// input lists, credentials and temporary downloads retain their roots.
		directory, err = filepath.Abs(directory)
		if err != nil {
			return "", "", fmt.Errorf("resolving --output-dir: %w", err)
		}
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return "", "", fmt.Errorf("creating --output-dir %q: %w", directory, err)
		}
	}
	return directory, name, nil
}

func episodeOutputPath(info EpisodeInfo, quality, directory, filename, format string) (string, error) {
	name, err := normalizeOutputFilename(filename, format)
	if err != nil {
		return "", err
	}
	series := sanitizeFilename(info.EpisodeMetadata.SeriesTitle)
	if name == "" {
		name = fmt.Sprintf("%s S%02dE%02d - %s [%s].%s", series,
			info.EpisodeMetadata.SeasonNumber, info.EpisodeMetadata.EpisodeNumber,
			sanitizeFilename(info.Title), quality, format)
	}
	if directory == "" {
		directory = series
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolving output directory: %w", err)
	}
	return filepath.Join(directory, name), nil
}
