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
	outputDir        = flag.String("output-dir", "", "Exact destination directory; absolute or relative to the working directory, including .. (default: series folder)")
	outputFilename   = flag.String("filename", "", "Literal output filename for a single episode, with optional matching container extension (default: generated episode name)")
	outputFormat     = flag.String("output-format", "mkv", "Output container: mkv (embedded subtitles) or mp4 (original subtitle files alongside video)")
	progressFormat   = flag.String("progress-format", "text", "Progress interface: text or json (CVA protocol v1, one episode, video/audio only)")
	inputURL         = flag.String("url", "", "URL of the episode/season to download")
	urlsFile         = flag.String("file", "", "Path to a text file with one URL per line")
	forcedSubsLang   = flag.String("forced-subs-lang", "", "Mark selected embedded MKV subtitle locales as forced, comma-separated (e.g. en-US); does not affect MP4 sidecar names or select/extract cues")
	seasonNumber     = flag.Int("season", 0, "Season number. Not used if an episode link is entered")
	etpRt            = flag.String("etp-rt", "", "The \"etp_rt\" cookie value of your account")
	saveCookie       = flag.Bool("save-cookie", false, "Validate and save --etp-rt using CVA's selected secure store for future downloads")
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

func processUrl(url string) error {
	contentType, contentId := parseUrl(url)
	if contentType == "" || contentId == "" {
		fmt.Printf("Invalid URL (must be /watch/ or /series/): %s\n", url)
		return failure("invalid_arguments", "resolving", errors.New("invalid content URL"))
	}

	audioLangs := parseLangs(*audioLang)
	if len(audioLangs) == 0 {
		audioLangs = []string{"ja-JP"}
	}
	subsLangs := parseLangs(*subtitlesLang)
	ccLangs := parseLangs(*ccLang)
	if events != nil {
		// CVA owns subtitles. Its invocation produces video/audio only, even
		// though interactive mode defaults to requesting English subtitles.
		subsLangs, ccLangs = nil, nil
		audioLangs = uniqueLangs(audioLangs)
	}

	// The season/series API endpoints take a single preferred locale; use the
	// first explicit request or the usual default for ALL alone. Track
	// availability is resolved from each episode's dub versions later.
	primaryAudio := preferredRequestLocale(audioLangs, "ja-JP")
	primarySubs := preferredRequestLocale(subsLangs, "en-US")

	if contentType == "watch" {
		return retryEpisode(func() error {
			info := getEpisodeInfo(contentId)
			return downloadEpisode(contentId, info, audioLangs, subsLangs, ccLangs, videoQuality, audioQuality)
		})
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
				return failure("input_failed", "resolving", errors.New("season unavailable"))
			}

			episodes := getSeasonEpisodes(seasonId, primaryAudio, primarySubs)
			return downloadSeason(videoQuality, audioQuality, audioLangs, subsLangs, ccLangs, episodes)
		} else {
			print("No season number specified, downloading all seasons...\n")
			var failures error
			for _, season := range seasons {
				episodes := getSeasonEpisodes(season.ID, primaryAudio, primarySubs)
				if err := downloadSeason(videoQuality, audioQuality, audioLangs, subsLangs, ccLangs, episodes); err != nil {
					failures = errors.Join(failures, err)
				}
			}
			return failures
		}
	}
}

func main() {
	os.Exit(runCLI(os.Args[1:]))
}

// Recognize JSON before parsing, so even an unknown flag preceding the mode
// option gets a protocol record and a sanitized terminal failure. Never echo
// argument values to the machine-readable stream.
func wantsJSON(args []string) bool {
	for i, arg := range args {
		if arg == "--" {
			break
		}
		name := strings.TrimPrefix(arg, "-")
		name = strings.TrimPrefix(name, "-")
		if name == "progress-format=json" || (name == "progress-format" && i+1 < len(args) && args[i+1] == "json") {
			return true
		}
	}
	return false
}

func runCLI(args []string) (status int) {
	return runCLIWith(args, runDownload)
}

// The invocation boundary owns stdout and the terminal event. Keeping it
// separate from the download allows offline process-level protocol tests.
func runCLIWith(args []string, download func() error) (status int) {
	var err error
	originalStdout := os.Stdout
	if wantsJSON(args) {
		events = newEventReporter(originalStdout, processStarted)
		// Legacy diagnostics and dependency output go to stderr. Only the
		// reporter retains the original stdout handle; it writes without buffering.
		os.Stdout = os.Stderr
	}
	defer func() {
		if r := recover(); r != nil {
			err = failure("internal_error", events.currentPhase(), fmt.Errorf("%v", r))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		status = events.finish(err, *outputFormat)
		os.Stdout = originalStdout
	}()
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(os.Stderr)
	if err = flag.CommandLine.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) && events == nil {
			err = nil
			return
		}
		err = failure("invalid_arguments", "initializing", err)
		return
	}
	if (*progressFormat != "text" && *progressFormat != "json") || (events != nil && *progressFormat != "json") || flag.NArg() > 0 {
		err = failure("invalid_arguments", "initializing", errors.New("use --progress-format text or json and named options only"))
		return
	}
	err = download()
	return
}

func runDownload() error {
	if *saveCookie && (*etpRt == "" || *credentialsCheck) {
		return failure("invalid_arguments", "initializing", errors.New("--save-cookie requires --etp-rt and a download or --auth-check"))
	}
	format, err := parseOutputFormat(*outputFormat)
	if err != nil {
		return failure("invalid_arguments", "initializing", err)
	}
	*outputFormat = format
	if events != nil && (*authCheck || *credentialsCheck || *debug) {
		return failure("invalid_arguments", "initializing", errors.New("JSON progress requires a download; auth checks and debug manifests are not supported"))
	}

	if *credentialsCheck {
		if *authCheck || *etpRt != "" || *inputURL != "" || *urlsFile != "" {
			return failure("invalid_arguments", "initializing", errors.New("--credentials-check cannot be combined with --auth-check, --etp-rt, --url or --file"))
		}
		backend, err := checkCVACredentials(cvaAuthOptions{Root: *cvaRoot, Python: *cvaPython})
		if err != nil {
			return failure("credential_storage_failed", "initializing", err)
		}
		fmt.Printf("Stored CVA credentials are readable (%s). No sign-in was attempted.\n", backend)
		return nil
	}

	if *inputURL == "" && *urlsFile == "" {
		flag.Usage()
		return failure("invalid_arguments", "initializing", errors.New("a URL or URLs file is required"))
	}

	urls, err := inputURLs(*inputURL, *urlsFile)
	if err != nil {
		return failure("input_failed", "initializing", err)
	}
	if events != nil {
		kind, id := parseUrl(urls[0])
		if len(urls) != 1 || kind != "watch" || id == "" {
			return failure("invalid_arguments", "initializing", errors.New("JSON progress requires exactly one /watch/ URL"))
		}
	}
	*outputDir, *outputFilename, err = prepareOutputOptions(*outputDir, *outputFilename, *outputFormat, urls)
	if err != nil {
		return failure("output_configuration_failed", "initializing", err)
	}
	accessTokenProvider, err = newAccessTokenProvider(*etpRt, cvaAuthOptions{
		Root: *cvaRoot, Python: *cvaPython, SourceURL: urls[0], SaveCookie: *saveCookie,
	})
	if err != nil {
		return failure("sign_in_configuration_failed", "initializing", err)
	}
	// The authentication check deliberately needs no FFmpeg or media files.
	if !*authCheck {
		ffmpeg, err := findFFmpeg()
		if err != nil {
			return failure("ffmpeg_failed", "initializing", err)
		}
		ffmpegPath = ffmpeg
	}

	events.setPhase("authenticating")
	token, err = accessTokenProvider()
	if err != nil {
		return operationFailure(err, "authentication_failed", "authenticating")
	}
	if *authCheck {
		fmt.Println("Authentication succeeded. No media was downloaded.")
		return nil
	}
	backoff = newDownloadBackoff(*downloadDelay)

	if *urlsFile != "" {
		fmt.Printf("Found %d URLs to download\n\n", len(urls))
		var failures error
		for i, u := range urls {
			fmt.Printf("=== [%d/%d] %s ===\n", i+1, len(urls), u)
			if err := processUrl(u); err != nil {
				failures = errors.Join(failures, err)
			}
			fmt.Println()
		}
		return failures
	} else {
		return processUrl(urls[0])
	}
}

func uniqueLangs(langs []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, lang := range langs {
		if !seen[lang] {
			result = append(result, lang)
			seen[lang] = true
		}
	}
	return result
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
