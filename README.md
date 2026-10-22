# Crunchyroll Downloader

[![Tests](https://img.shields.io/github/actions/workflow/status/gearheart89/crunchyroll-downloader/tests.yml?branch=feature/cva-keyring-auth&label=tests)](https://github.com/gearheart89/crunchyroll-downloader/actions/workflows/tests.yml)
[![Latest release](https://img.shields.io/github/v/release/CuteTenshii/crunchyroll-downloader)](https://github.com/CuteTenshii/crunchyroll-downloader/releases/latest)
[![Downloads](https://img.shields.io/github/downloads/CuteTenshii/crunchyroll-downloader/total)](https://github.com/CuteTenshii/crunchyroll-downloader/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/CuteTenshii/crunchyroll-downloader)](go.mod)
[![License](https://img.shields.io/github/license/CuteTenshii/crunchyroll-downloader)](LICENSE.txt)

Downloads anime from Crunchyroll and outputs them in a MKV file.

This fork starts from upstream **1.6.0** (`182ea1b`) and adds Windows sign-in
using the account/password already stored by **Crunchyroll Video Analyzer
(CVA)** in the operating-system keyring. Build this fork to use the integration;
the upstream release downloads above do not contain it.

## CVA keyring sign-in

On Windows, omit `--etp-rt` to use CVA sign-in automatically. This requires:

- An existing CVA installation/source folder and its Python dependencies.
- The Python interpreter used by CVA (`keyring`, `websocket-client`, and the other
  packages in CVA's own `requirements.txt` must be installed for that interpreter).
- Installed official Google Chrome and a Windows environment supported by CVA.
- Credentials previously added through CVA's credential manager, under the same
  Windows user running the downloader.

The fork imports CVA's existing `KeyringCredentialStore` and hosted browser SSO
flow. The shared keyring service is `Crunchyroll Video Analyzer/Crunchyroll`,
with `account` and `password` slots. The downloader only reads those entries;
manage or update them through CVA.

CVA is discovered in a sibling folder named `Crunchyroll Video Analyzer`, or
through `--cva-root` / `CRUNCHYROLL_CVA_ROOT`. Select its Python executable with
`--cva-python` / `CRUNCHYROLL_CVA_PYTHON`; otherwise `python` on PATH is used.
Explicit flags take precedence over environment settings.

Check sign-in without downloading anything (FFmpeg and Widevine files are not
needed for this check):

```powershell
.\crunchyroll-downloader.exe --auth-check --url https://www.crunchyroll.com/series/GJ0H7Q5ZJ/hells-paradise --cva-root "..\Crunchyroll Video Analyzer"
```

Then use the same command without `--auth-check`, adding your download options:

```powershell
.\crunchyroll-downloader.exe --url https://www.crunchyroll.com/series/GJ0H7Q5ZJ/hells-paradise --season 1 --audio-lang ja-JP --subs-lang en-US
```

The downloader uses its own persistent profile and runtime state under
`%LOCALAPPDATA%\Crunchyroll Downloader`, separate from CVA's profile. Each token
acquisition starts and closes its owned Chrome runtime. Existing session state
is tried first; keyring is consulted only when normal first-party sign-in is
needed. The account/password stay in Python memory and the OS keyring. Only a
short-lived bearer reaches Go through a private process pipe; no credentials or
tokens are written to downloader configuration or logs. Browser session cookies
remain in the dedicated browser profile. Initial sign-in and token refresh use
the same provider. Run only one downloader instance with CVA sign-in at a time.

If the service requires CAPTCHA, MFA, consent, or another interactive step, the
bridge stops with a sanitized error. Complete normal sign-in in your browser and
use the original `--etp-rt` option for that run. An explicit nonempty `--etp-rt`
always selects the original cookie flow and does not require CVA or Python.
Linux and macOS retain that original cookie flow.

The bridge depends on CVA's internal Python APIs. Offline compatibility was
checked against the local CVA revision `18cc783`; CVA updates may require
corresponding adapter changes. Tests use fake credentials and local HTTP servers.
On 2026-10-22, the built Windows executable also passed a live `--auth-check`
using the existing keyring credentials and its separate Chrome profile. That
check performed no media download; it does not validate the download pipeline.

## Features

- Supports choosing the audio and subtitles language, including downloading multiple of each into a single file
- Supports choosing the audio and video quality
- Decrypts Widevine DRM (requires: a `.wvd` file or `client_id.bin` and `private_key.pem` files)
- Adds metadata (like episode name) to the MKV container
- Parallel segment downloads (10 workers) for faster downloads
- Retry with backoff on connection errors
- Batch download from a list of URLs

## Requirements

- [FFmpeg](https://www.ffmpeg.org/download.html#get-packages)
- To download Premium-only content, a Crunchyroll Premium account. No, this can't be bypassed and a free trial should be enough
- Either a `.wvd` file, or a `client_id.bin` and `private_key.pem`

## Download

Check the [latest release](https://github.com/CuteTenshii/crunchyroll-downloader/releases/latest) and download the file that corresponds to your OS.

## Usage

- Open a Terminal/Command prompt, and go to the folder where you downloaded the binary/cloned the repo
- Run the program with the options you want:
```shell
Usage of ./crunchyroll-downloader:
  -auth-check
        Check authentication and exit without downloading (requires --url or --file)
  -audio-lang string
        Audio language(s), comma-separated (e.g. "ja-JP,en-US"). Add ALL for every available dub; explicit languages lead, the first available track is default, and ALL alone prefers ja-JP (default "ja-JP")
  -audio-quality string
        Audio quality (default "192k")
  -cc-lang string
        Closed caption language(s), comma-separated (e.g. "en-US,ALL"). Add ALL for every available caption; downloaded in addition to --subs-lang
  -cva-python string
        Python executable with CVA dependencies (default: CRUNCHYROLL_CVA_PYTHON or python)
  -cva-root string
        Crunchyroll Video Analyzer folder (default: sibling folder or CRUNCHYROLL_CVA_ROOT)
  -debug-manifest
        Log raw episode playback JSON and manifest XML
  -download-delay duration
        Minimum delay between episode downloads, to help avoid Crunchyroll's rate limiting (e.g. "30s", "2m")
  -etp-rt string
        The "etp_rt" cookie value of your account
  -file string
        Path to a text file with one URL per line
  -season int
        Season number. Not used if an episode link is entered
  -subs-lang string
        Subtitle language(s), comma-separated (e.g. "fr-FR,ALL"). Add ALL for every available subtitle; explicit languages lead, the first available track is default, and ALL alone prefers en-US (default "en-US")
  -url string
        URL of the episode/season to download
  -video-quality string
        Video quality (default "1080p")
```

The examples below use the original cookie option. On Windows with CVA set up,
omit `--etp-rt replace_this` to use keyring sign-in instead.

Ex: to download the first season of *Hell's Paradise*:
```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/series/GJ0H7Q5ZJ/hells-paradise --season 1 --etp-rt replace_this
```

To download a specific episode:
```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/watch/GE00198973JAJP/dawn-and-confusion --etp-rt replace_this
```

To batch download from a file (one URL per line):
```shell
./crunchyroll-downloader --file list.txt --etp-rt replace_this --subs-lang pt-BR
```

To download multiple audio tracks and subtitles into a single file (the first available requested track of each kind is set as the default):
```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/watch/GE00198973JAJP/dawn-and-confusion --etp-rt replace_this --audio-lang ja-JP,en-US --subs-lang en-US,es-419,de-DE
```

Add `ALL` to any language list to include every available language; listed languages come first. For example, `--subs-lang fr-FR,ALL` makes French the default subtitle and adds the rest:
```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/watch/GE00198973JAJP/dawn-and-confusion --etp-rt replace_this --audio-lang ja-JP,ALL --subs-lang fr-FR,ALL --cc-lang ALL
```

If you're getting rate-limited while downloading a season/batch, wait at least this long between each episode:
```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/series/GJ0H7Q5ZJ/hells-paradise --season 1 --etp-rt replace_this --download-delay 30s
```

If Crunchyroll rate-limits an episode anyway, it's retried in place (starting at `-download-delay`, or 1 minute if unset, doubling up to 30 minutes on repeated hits) instead of moving on to the next episode and tripping the same limit again.

## Building

### Requirements

- [Go](https://go.dev/dl/)

### Guide

- Clone this repository
- Open a Terminal/Command prompt, and go to the folder where you cloned the repo
- Run `go build .`

The Python bridge is embedded in the binary; keep the existing CVA installation
available at runtime. No additional Go dependency was added.

### Tests

```shell
go test -count=2 -timeout 5m ./...
go vet ./...
python -B -m unittest discover -s tests -v
```

For offline compatibility checks against your installed CVA, set `CVA_TEST_ROOT`
to that folder before running the Python tests. These checks import its classes
with fake browser/keyring implementations and never access the real credential
store. The CI Go suite also runs with the race detector.

## Help

### How do I get my `etp_rt` cookie?

- Go to https://crunchyroll.com
- Open Developer Tools
- Firefox: Go to *Storage* then *Cookies*<br />Chrome: Go to *Application* then *Cookies*
- Select the Crunchyroll domain, then copy the `etp_rt` cookie value

![](.github/screenshots/etp-rt-cookie.png)

### What is a `.wvd` file and do I really need one?

Yes, Crunchyroll uses DRM-only content. This file is used to get a Widevine license, which gives the keys to decrypt the media.

If you don't have a rooted Android device or are just lazy, search "ready to use cdms" and you'll find plenty of websites providing those files.

## License

This project is licensed under the MIT License. See [LICENSE.txt](LICENSE.txt)
