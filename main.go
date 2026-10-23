package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

var (
	token            = ""
	audioLang        = flag.String("audio-lang", "ja-JP", "Audio language(s), comma-separated (e.g. \"ja-JP,en-US\"). Add ALL for every available dub; explicit languages lead, the first available track is default, and ALL alone prefers ja-JP")
	subtitlesLang    = flag.String("subs-lang", "en-US", "Subtitle language(s), comma-separated (e.g. \"fr-FR,ALL\"). Add ALL for every available subtitle; explicit languages lead, the first available track is default, and ALL alone prefers en-US")
	ccLang           = flag.String("cc-lang", "", "Closed caption language(s), comma-separated (e.g. \"en-US,ALL\"). Add ALL for every available caption; downloaded in addition to --subs-lang")
	videoQuality     = flag.String("video-quality", "1080p", "Video quality")
	audioQuality     = flag.String("audio-quality", "192k", "Audio quality")
	outputFormat     = flag.String("output-format", "mkv", "Output container: mkv (embedded subtitles) or mp4 (original subtitle files alongside video)")
	forcedSubsLang   = flag.String("forced-subs-lang", "", "Mark selected subtitle locales as forced, comma-separated (e.g. en-US); labels whole tracks, does not select or extract cues")
	seasonNumber     = flag.Int("season", 0, "Season number. Not used if an episode link is entered")
	etpRt            = flag.String("etp-rt", "", "The \"etp_rt\" cookie value of your account")
	cvaRoot          = flag.String("cva-root", "", "Crunchyroll Video Analyzer folder (default: sibling folder or CRUNCHYROLL_CVA_ROOT)")
	cvaPython        = flag.String("cva-python", "", "Python executable with CVA dependencies (default: CRUNCHYROLL_CVA_PYTHON, then python on Windows or python3 elsewhere)")
	authCheck        = flag.Bool("auth-check", false, "Check authentication and exit without downloading (requires --url or --file)")
	credentialsCheck = flag.Bool("credentials-check", false, "Check readability of CVA's selected credential store without signing in (no URL required)")
	debug            = flag.Bool("debug-manifest", false, "Log raw episode playback JSON and manifest XML")
	downloadDelay    = flag.Duration("download-delay", 0, "Minimum delay between episode downloads, to help avoid Crunchyroll's rate limiting (e.g. \"30s\", \"2m\")")
)

// backoff spaces out consecutive episode downloads by *downloadDelay. It is
// initialized in main() once flags are parsed, and is a no-op (including when
// left nil) whenever no delay is configured.
var backoff *downloadBackoff

// parseLangs splits a comma-separated locale list, trimming spaces and dropping
// empties.
func parseLangs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// preferredRequestLocale provides a real locale for the season API even when
// ALL is requested; track selection still happens against each episode later.
func preferredRequestLocale(langs []string, fallback string) string {
	for _, locale := range langs {
		if !strings.EqualFold(locale, "all") {
			return locale
		}
	}
	return fallback
}

// parseUrl extracts the content type ("watch" or "series") and content ID from a
// Crunchyroll URL, tolerating an optional locale prefix such as "/fr/". The
// content type and ID may appear at any position after the host.
func parseUrl(url string) (contentType, contentId string) {
	parts := strings.Split(strings.TrimRight(url, "/"), "/")
	for i, p := range parts {
		if (p == "watch" || p == "series") && i+1 < len(parts) {
			return p, parts[i+1]
		}
	}
	return "", ""
}

func processUrl(url string) {
	contentType, contentId := parseUrl(url)
	if contentType == "" || contentId == "" {
		fmt.Printf("Invalid URL (must be /watch/ or /series/): %s\n", url)
		return
	}

	audioLangs := parseLangs(*audioLang)
	if len(audioLangs) == 0 {
		audioLangs = []string{"ja-JP"}
	}
	subsLangs := parseLangs(*subtitlesLang)
	ccLangs := parseLangs(*ccLang)

	// The season/series API endpoints take a single preferred locale; use the
	// first explicit request or the usual default for ALL alone. Track
	// availability is resolved from each episode's dub versions later.
	primaryAudio := preferredRequestLocale(audioLangs, "ja-JP")
	primarySubs := preferredRequestLocale(subsLangs, "en-US")

	if contentType == "watch" {
		info := getEpisodeInfo(contentId)
		downloadEpisodeWithRetry(contentId, info, audioLangs, subsLangs, ccLangs, videoQuality, audioQuality)
	} else {
		seasons := getSeasons(contentId, primaryAudio, primarySubs)

		if *seasonNumber != 0 {
			var seasonId string
			for _, season := range seasons {
				if season.SeasonNumber == *seasonNumber {
					seasonId = season.ID
					break
				}
			}
			if seasonId == "" {
				fmt.Printf("This anime has no season %v!\n", *seasonNumber)
				return
			}

			episodes := getSeasonEpisodes(seasonId, primaryAudio, primarySubs)
			downloadSeason(videoQuality, audioQuality, audioLangs, subsLangs, ccLangs, episodes)
		} else {
			print("No season number specified, downloading all seasons...\n")

			for _, season := range seasons {
				episodes := getSeasonEpisodes(season.ID, primaryAudio, primarySubs)
				downloadSeason(videoQuality, audioQuality, audioLangs, subsLangs, ccLangs, episodes)
			}
		}
	}
}

func main() {
	url := flag.String("url", "", "URL of the episode/season to download")
	urlsFile := flag.String("file", "", "Path to a text file with one URL per line")
	flag.Parse()
	format, err := parseOutputFormat(*outputFormat)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	*outputFormat = format

	if *credentialsCheck {
		if *authCheck || *etpRt != "" || *url != "" || *urlsFile != "" {
			fmt.Println("--credentials-check cannot be combined with --auth-check, --etp-rt, --url or --file.")
			os.Exit(1)
		}
		backend, err := checkCVACredentials(cvaAuthOptions{Root: *cvaRoot, Python: *cvaPython})
		if err != nil {
			fmt.Printf("Couldn't read stored credentials: %s\n", err)
			os.Exit(1)
		}
		fmt.Printf("Stored CVA credentials are readable (%s). No sign-in was attempted.\n", backend)
		return
	}

	if *url == "" && *urlsFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	urls, err := inputURLs(*url, *urlsFile)
	if err != nil {
		fmt.Printf("Couldn't read input: %s\n", err)
		os.Exit(1)
	}
	accessTokenProvider, err = newAccessTokenProvider(*etpRt, cvaAuthOptions{
		Root: *cvaRoot, Python: *cvaPython, SourceURL: urls[0],
	})
	if err != nil {
		fmt.Printf("Couldn't configure sign-in: %s\n", err)
		os.Exit(1)
	}
	// The authentication check deliberately needs no FFmpeg or media files.
	if !*authCheck {
		ffmpeg, err := findFFmpeg()
		if err != nil {
			fmt.Println("FFmpeg wasn't found. Install it and add it to your PATH, or put the ffmpeg executable in the same folder as this program.\nDownload it from https://www.ffmpeg.org/download.html")
			os.Exit(1)
		}
		ffmpegPath = ffmpeg
	}

	token, err = accessTokenProvider()
	if errors.Is(err, errAuthRejected) {
		fmt.Printf("Couldn't log in: %s\nYour \"-etp-rt\" cookie is wrong or expired. Copy a fresh one from your browser (see the README).\n", err)
		os.Exit(1)
	}
	if err != nil {
		fmt.Printf("Couldn't log in: %s\n", err)
		os.Exit(1)
	}
	if *authCheck {
		fmt.Println("Authentication succeeded. No media was downloaded.")
		return
	}
	backoff = newDownloadBackoff(*downloadDelay)

	if *urlsFile != "" {
		fmt.Printf("Found %d URLs to download\n\n", len(urls))
		for i, u := range urls {
			fmt.Printf("=== [%d/%d] %s ===\n", i+1, len(urls), u)
			processUrl(u)
			fmt.Println()
		}
	} else {
		processUrl(urls[0])
	}
}

func inputURLs(url, filename string) ([]string, error) {
	if filename == "" {
		if strings.TrimSpace(url) == "" {
			return nil, errors.New("a URL or URLs file is required")
		}
		return []string{strings.TrimSpace(url)}, nil
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var urls []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "http") {
			urls = append(urls, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(urls) == 0 {
		return nil, errors.New("the URLs file contains no URLs")
	}
	return urls, nil
}
