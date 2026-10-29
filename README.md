# Crunchyroll Downloader

[![Tests](https://img.shields.io/github/actions/workflow/status/gearheart89/crunchyroll-downloader/tests.yml?branch=feature/cva-keyring-auth&label=tests)](https://github.com/gearheart89/crunchyroll-downloader/actions/workflows/tests.yml)
[![Latest fork release](https://img.shields.io/github/v/release/gearheart89/crunchyroll-downloader)](https://github.com/gearheart89/crunchyroll-downloader/releases/latest)
[![Fork downloads](https://img.shields.io/github/downloads/gearheart89/crunchyroll-downloader/total)](https://github.com/gearheart89/crunchyroll-downloader/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/CuteTenshii/crunchyroll-downloader)](go.mod)
[![License](https://img.shields.io/github/license/CuteTenshii/crunchyroll-downloader)](LICENSE.txt)

Downloads anime from Crunchyroll and outputs them in an MKV or MP4 file.

This fork starts from upstream **1.6.0** (`182ea1b`) and reads the account/password
already stored by **Crunchyroll Video Analyzer (CVA)** through its platform-aware
credential store. Windows browser sign-in uses those stored credentials. Use
the fork release **`cva/v1.6.0-1`** or build this fork to use the integration;
upstream releases do not contain these customizations.

For future upstream reviews, customization requirements, validation, and parent
project handoff, see the [fork maintenance guide](MAINTENANCE.md).

## CVA stored credentials

Omit `--etp-rt` to try the downloader's securely stored session cookie first.
On Windows, a missing, expired, or rejected cookie is renewed through CVA's
browser sign-in and the replacement is saved automatically. This requires:

- An existing CVA installation/source folder and its Python dependencies.
- The Python interpreter used by CVA (`keyring`, `websocket-client`, and the other
  packages in CVA's own `requirements.txt` must be installed for that interpreter).
- Installed official Google Chrome and a Windows environment supported by CVA
  when browser renewal is needed; a valid stored cookie does not launch Chrome.
- Credentials previously added through CVA's credential manager, under the same
  Windows user running the downloader.

The fork calls CVA's `select_credential_store()` and
`default_credential_store(selection=...)` directly. It uses the same detection,
configuration, and storage implementation as CVA, without a separate password
setup. The Windows keyring service remains
`Crunchyroll Video Analyzer/Crunchyroll`, with `account` and `password` slots.
Manage or update account/password credentials through CVA; the downloader does
not overwrite or delete that pair. Its session cache uses a separate namespace
or vault file through the same CVA storage implementation.

CVA is discovered in a sibling folder named `Crunchyroll Video Analyzer`, or
through `--cva-root` / `CRUNCHYROLL_CVA_ROOT`. Select its Python executable with
`--cva-python` / `CRUNCHYROLL_CVA_PYTHON`; otherwise `python` on PATH is used
on Windows and `python3` (falling back to `python`) on other systems.
Explicit flags take precedence over environment settings.

### Credential check and Unraid/container storage

`--credentials-check` verifies that the selected store returns a complete
credential pair and exits without opening Chrome, contacting Crunchyroll, or
displaying the account/password. It needs no URL, FFmpeg, or media files. It
checks stored-credential readability, not whether Crunchyroll accepts them.
Use it separately from `--auth-check`, `--etp-rt`, `--url`, and `--file`.

```powershell
.\crunchyroll-downloader.exe --credentials-check --cva-root "..\Crunchyroll Video Analyzer"
```

CVA's automatic selection uses the OS keyring on ordinary Windows/POSIX desktops.
`CVA_CONTAINER=true` (also `1` or `yes`), `/.dockerenv`, or `/run/.containerenv`
selects the encrypted container vault. The explicit
`CVA_CREDENTIAL_BACKEND=os-keyring` or `container-vault` override takes precedence;
`auto` is the default. Invalid settings fail with an error.

For Unraid/Docker, expose the same CVA vault and unlock-key files to the downloader
and run it as the same dedicated effective UID. Keep the vault directory private
(`0700`), the vault and its sibling lock file `0600`, and the key `0400` or `0600`.
The vault directory must permit access to the sibling lock even when only
reading credentials; the key can be mounted read-only. The key contains exactly
32 raw bytes provisioned through CVA's documented setup. Do not put credentials
or key bytes in these environment settings; their values are paths only.

Example inside a prepared Linux environment (a Docker image is not required):

```sh
export CRUNCHYROLL_CVA_ROOT=/opt/cva
export CVA_CONTAINER=true
export CVA_CREDENTIAL_VAULT_PATH=/var/lib/cva/credentials.vault
export CVA_CREDENTIAL_KEY_PATH=/run/secrets/cva-credential-vault-key
./crunchyroll-downloader --credentials-check --cva-python /opt/cva-venv/bin/python
```

Those vault/key paths are CVA's defaults; change them to match the paths visible
inside your environment. Install the updated CVA's declared Python dependencies,
including `cryptography==50.0.2`, in the chosen interpreter. The authoritative
storage/unlock contract is `docs/CONTAINER_CREDENTIAL_VAULT.md` in your CVA folder.
Missing credentials, locked keys, corrupt vaults, unavailable storage, and invalid
configuration produce distinct sanitized errors; there is no plaintext fallback.

**Stored-cookie authentication works on Linux/Docker. Browser renewal through
CVA still requires Windows.** A missing or expired Docker cookie produces a
clear authentication failure until a replacement is supplied. This change does
not provide a Linux Chrome lifecycle or a Docker application image.

### Session cookie reuse and renewal

Normal invocations, including `--auth-check`, use this sequence:

1. Read the downloader session from CVA's selected storage backend.
2. Exchange a valid stored `etp_rt` cookie for a short-lived bearer token. Keep
   its device ID stable and save cookie/device rotations returned by the service.
3. If the cookie is missing, past its recorded expiry, or explicitly rejected
   by authentication, renew it through the existing Windows/CVA browser flow.
4. Validate the renewed cookie with the token endpoint before replacing the
   stored session. A failed renewal preserves the previous saved value.

An expired bearer during a download uses the same cookie-first provider.
Parallel audio/video workers share bearer refreshes. Connection failures,
rate limits, server failures, and generic 403 challenge responses do not prove
cookie expiry and do not launch Chrome or overwrite the session. Cookie expiry
is taken from cookie metadata when available; a bearer token's `expires_in`
does not determine cookie lifetime. Unknown cookie lifetime is checked through
the token endpoint on each invocation.

Storage locations are:

| Platform/backend | Downloader session storage |
| --- | --- |
| Windows / OS keyring | Service `Crunchyroll Downloader/Crunchyroll Session`, using CVA's `KeyringCredentialStore` |
| Unraid / Docker vault | `<CVA_CREDENTIAL_VAULT_PATH>.downloader-session`, using CVA's `ContainerCredentialVault` and the same `CVA_CREDENTIAL_KEY_PATH` |

The cache holds only the session cookie, device ID, and optional expiry. To
reuse CVA's existing pair-based API without changing CVA, the separate store's
`account` slot contains a fixed session schema marker and its `password` slot
contains the session payload. The original CVA account/password store is never
repurposed. There is no plaintext cache or fallback.

With the default container settings, the new encrypted file is
`/var/lib/cva/credentials.vault.downloader-session`, with its own `.lock` file.
Mount the **directory** persistently and writable so both the session and its
lock survive container restarts; mounting only `credentials.vault` does not
persist the downloader cache. CVA enforces the same private directory/file
permissions, authenticated encryption, atomic writes, and owner checks. The
existing unlock key may remain mounted read-only.

To seed or replace a session manually, add `--save-cookie` to `--etp-rt`. This
requires CVA/Python and validates the supplied cookie before saving it. For
example, inside the configured Docker/Linux environment:

```sh
./crunchyroll-downloader --auth-check --url "https://www.crunchyroll.com/watch/EPISODE_ID" --etp-rt "COOKIE_VALUE" --save-cookie --cva-python /opt/cva-venv/bin/python
```

Later downloads omit both cookie options:

```sh
./crunchyroll-downloader --url "https://www.crunchyroll.com/watch/EPISODE_ID" --progress-format json --output-format mp4 --cva-python /opt/cva-venv/bin/python
```

The same flags work on Windows with `crunchyroll-downloader.exe`. Treat a supplied
cookie like a password; command-line arguments and shell history can expose it.
An explicit cookie without `--save-cookie` remains a one-run override and requires
neither CVA nor Python. A rejected explicit cookie is reported as an error rather
than silently switching accounts. When switching accounts, explicitly replace
the downloader session too; it is independent of CVA's account/password pair.
`--credentials-check` still checks that pair, while `--auth-check` exercises the
actual cookie-first sign-in path. Use one downloader process per session store;
within a process, renewal and cache updates are serialized.

### Windows browser sign-in

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
`%LOCALAPPDATA%\Crunchyroll Downloader`, separate from CVA's profile. Chrome is
started and closed only when cookie renewal is necessary. Existing browser
session state is tried first; account/password credentials are read only when
normal first-party sign-in is needed. A rejected/expired cookie is removed only
from this downloader-owned browser profile before renewal, so it is not simply
recaptured. The new cookie is read through Chrome's
[CDP cookie API](https://chromedevtools.github.io/devtools-protocol/tot/Network/#method-getCookies)
before the runtime closes. Account/password stay in Python and CVA's store;
session secrets pass between Python and Go through private pipes, never logs or
progress events. Run only one downloader instance with CVA sign-in at a time.

If the service requires CAPTCHA, MFA, consent, or another interactive step, the
bridge stops with a sanitized error. Complete normal sign-in in your browser and
use `--etp-rt` for that run, adding `--save-cookie` to preserve it for future runs.

The bridge depends on CVA's internal Python APIs, including its completed
platform selector, factory, and container vault. Use the updated CVA working
copy containing those APIs; the older `18cc783` committed snapshot alone does not
contain them. Tests use fake credentials and local HTTP servers.
On 2026-10-22, the built Windows executable also passed a live `--auth-check`
using the existing keyring credentials and its separate Chrome profile. That
check performed no media download; it does not validate the download pipeline.
The platform-aware update was also tested on Windows and native Ubuntu WSL with
CVA's pinned dependencies, including a compiled Linux downloader reading a
temporary CVA-created encrypted vault. These checks used only synthetic
credentials and do not claim a deployed Unraid/Docker application test.

## Features

- Supports choosing the audio and subtitles language, including downloading multiple of each into a single file
- Supports choosing the audio and video quality
- Decrypts Widevine DRM (requires: a `.wvd` file or `client_id.bin` and `private_key.pem` files)
- Outputs MKV with embedded subtitles, or MP4 with subtitles in separate files
- Adds metadata (like episode name) to the output container
- Parallel segment downloads (10 workers) for faster downloads
- Retry with backoff on connection errors
- Batch download from a list of URLs

## Requirements

- [FFmpeg](https://www.ffmpeg.org/download.html#get-packages)
- To download Premium-only content, a Crunchyroll Premium account. No, this can't be bypassed and a free trial should be enough
- Either a `.wvd` file, or a `client_id.bin` and `private_key.pem`

## Download

Check the [latest fork release](https://github.com/gearheart89/crunchyroll-downloader/releases/latest) and download the file that corresponds to your OS.

| Platform | Release asset |
| --- | --- |
| Windows x64 | `crdl-windows.exe` |
| Debian/Linux x64, including Docker | `crdl-linux` |
| macOS Apple Silicon | `crdl-macos-arm64` |
| macOS Intel | `crdl-macos-x64` |

Rename the Windows asset to `crunchyroll-downloader.exe` or the Linux/macOS
asset to `crunchyroll-downloader` to follow the commands below. On Linux/macOS,
run `chmod +x crunchyroll-downloader` after downloading it.

These assets contain the downloader and embedded bridge, not FFmpeg, CVA,
Python, or device credentials. CVA authentication still requires a compatible
CVA source installation and Python runtime with its dependencies. Automatic
browser sign-in renewal currently requires Windows; Linux/Docker uses a saved
cookie or an explicit `--etp-rt` cookie as described above.

Fork tags use `cva/v<upstream-version>-<fork-revision>`, starting with
`cva/v1.6.0-1`. Upstream's `1.6.0` tag remains separate. See the
[changelog](CHANGELOG.md) for the changes included in this snapshot.

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
  -credentials-check
        Check readability of CVA's selected credential store without signing in (no URL required)
  -cva-python string
        Python executable with CVA dependencies (default: CRUNCHYROLL_CVA_PYTHON, then python on Windows or python3 elsewhere)
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
  -filename string
        Literal output filename for a single episode, with optional matching container extension (default: generated episode name)
  -forced-subs-lang string
        Mark selected embedded MKV subtitle locales as forced, comma-separated (e.g. en-US); does not affect MP4 sidecar names or select/extract cues
  -output-dir string
        Exact destination directory; absolute or relative to the working directory, including .. (default: series folder)
  -output-format string
        Output container: mkv (embedded subtitles) or mp4 (original subtitle files alongside video) (default "mkv")
  -progress-format string
        Progress interface: text or json (CVA protocol v1, one episode, video/audio only) (default "text")
  -save-cookie
        Validate and save --etp-rt using CVA's selected secure store for future downloads
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

### Custom output directory and filename

`--output-dir` sets the exact destination folder; no series subfolder is added.
Missing directories are created. Absolute paths and relative paths with `..`
are supported using the operating system's normal path rules. Relative paths
start from the downloader process's working directory, which the calling
program can set; they are not relative to the executable or URL-list file.
Windows accepts drive and UNC paths, and either slash style. Linux uses `/`.

`--filename` is a literal filename supplied by the caller after expanding its
own template. Pass a basename, or include `.mp4`/`.mkv` matching
`--output-format`; a conflicting container extension is an error. The downloader
does not expand template placeholders or silently sanitize the supplied name.
Directory components go in `--output-dir`, including any `..` navigation.
Names containing path separators, control characters, `:*?"<>|`, trailing spaces
or dots, or names disallowed by the operating system are rejected.
When calling from another program, pass each option and value as a separate
argument-list entry. The shell quotes shown below are not part of the values.

Windows example:

```powershell
.\crunchyroll-downloader.exe --url "https://www.crunchyroll.com/watch/GE00198973JAJP/dawn-and-confusion" --output-dir "..\..\Media\Hell's Paradise\Season 01" --filename "Hell's Paradise - S01E01 - Dawn and Confusion.mp4" --output-format mp4
```

Debian/Linux container example:

```sh
./crunchyroll-downloader --url "https://www.crunchyroll.com/watch/GE00198973JAJP/dawn-and-confusion" --output-dir "../../media/Hell's Paradise/Season 01" --filename "Hell's Paradise - S01E01 - Dawn and Confusion" --output-format mp4
```

In Docker, paths refer to the container filesystem. For example, use
`--output-dir /downloads/Show/Season01` when the destination is mounted at
`/downloads`. Parent navigation follows the container directory tree; it does
not expose host directories that have not been mounted. The downloader leaves
its working directory unchanged, so relative input/CDM paths still use that
directory. Temporary media downloads use the operating system's temporary
directory; output staging uses the destination directory. Both locations must
be writable and have sufficient free space.

A literal `--filename` requires one `/watch/` episode URL (directly or in a
one-entry `--file`). For an external template-driven caller, invoke once per
episode. `--output-dir` alone also works for series and batches with generated
filenames. Omitting `--output-dir` retains the series folder under the working
directory; omitting `--filename` retains the generated episode name. Existing
output files are skipped as before.

MP4 subtitle files automatically use the final video basename, for example
`Hell's Paradise - S01E01 - Dawn and Confusion.en.ass`, with their source format
preserved.

### Machine-readable progress for CVA

Add `--progress-format json` to use **CVA Downloader Progress Protocol v1**.
Pass options and values as separate process arguments; do not combine stderr
with stdout. For example, a Linux caller can pass this argument array:

```json
["--url", "https://www.crunchyroll.com/watch/EPISODE_ID", "--progress-format", "json", "--output-format", "mp4", "--output-dir", "../stage2/My Show/Season 01", "--filename", "work-item", "--audio-lang", "ja-JP,en-US"]
```

Windows uses the same options with native paths, for example
`["--output-dir", "D:\\Media\\My Show\\Season 01"]`. Authentication continues
to use the existing CVA bridge or the explicitly supplied cookie.

JSON mode requires exactly one `/watch/` URL, either directly or in a one-entry
`--file`. It rejects series/batch requests, authentication-only checks, and
`--debug-manifest`. It downloads **video and audio only**: `--subs-lang`,
`--cc-lang`, and `--forced-subs-lang` have no effect in this mode, including the
usual English-subtitle default. CVA owns subtitle handling. Every explicitly
requested audio locale must be available; otherwise the invocation fails with
`requested_audio_unavailable`. Repeated audio locales are downloaded once.
`--output-format` still defaults to MKV; CVA should explicitly request MP4.

Stdout contains only UTF-8 JSON Lines, one complete event per newline, written
immediately. Stderr is diagnostic text, never a lifecycle signal. Events share
`protocol: "cva-downloader-progress"`, `version: 1`, a consecutive zero-based
`sequence`, nonnegative integer `elapsed_ms` measured from startup, and `event`.
The initial `protocol` record includes `tool: "crunchyroll-downloader"`.

| Event | Additional fields / meaning |
| --- | --- |
| `attempt_started` | `attempt`, starting at 1; resets per-track progress |
| `phase` | `phase`: `initializing`, `authenticating`, `resolving`, `downloading`, `decrypting`, `muxing`, `finalizing`, or `cleanup`; authentication steps also include `auth_step` and a safe `display_message` |
| `track_started` | `attempt`, `track_id`, `kind`, `locale`; IDs are `video:0` or `audio:<locale>`; video locale is null |
| `track_progress` | Same track identity plus integer `percent` (0–100), `segments_current`, `segments_total`, and `mbps` |
| `attempt_failed` | `attempt`, `code`, `recoverable: true`, fixed safe `display_message`; emitted only when retrying |
| `retry_wait` | `attempt`, `next_attempt`, `reason`, positive integer `delay_seconds`; follows `attempt_failed` |
| `warning` | Fixed `code` and safe `display_message`; not terminal |
| `completed` | `attempt`, absolute `output_path`, `output_format`, `cleanup_status`, `warnings` array; exits 0 |
| `skipped_existing` | `attempt`, absolute `output_path`, `validation: "existence_only"`; exits 0 |
| `failed` | `attempt`, failure `phase`, fixed `code`, safe `display_message`, and optional authentication `reason`; exits nonzero |

Percentages never decrease within a track/attempt. Segmented transfers report
integer counts with a positive total; single-file transfers report both segment
fields as null. Unknown-size transfers stay at 0 until successful transfer
completion. Mbps is a finite nonnegative number or null when unavailable.
100 means that track's transfer finished, not that the output is complete.
Parallel tracks may enter download/decryption at different times, so phases can
repeat while another track is still transferring.

The existing rate-limit retries emit `attempt_failed` / `retry_wait` with
`rate_limited`; the delay doubles from at least 60 seconds to a 30-minute cap.
There is no new automatic episode retry for other failures. Bounded segment
retries remain silent. Downloads do not resume partial files: a retry starts a
new attempt; an existing final path is only an existence-based skip.

`completed` is emitted after FFmpeg has exited, the staged output has been
published, workers have joined, and cleanup has finished. `cleanup_status` is
`confirmed` when opened player streams were removed, `not_applicable` when
there were none, or `warning` if any cleanup failed. Cleanup warnings emit
`cleanup_failed` and include that code in the terminal `warnings` array.
The protocol also permits `unknown`. Warning codes remain cumulative across
retries so earlier cleanup problems are not hidden.

Terminal failure codes are `invalid_arguments`, `input_failed`,
`output_configuration_failed`, `sign_in_configuration_failed`,
`authentication_failed`, `credential_storage_failed`,
`requested_audio_unavailable`, `network_failed`, `response_failed`,
`body_read_failed`, `download_failed`, `decryption_failed`, `ffmpeg_failed`,
`output_validation_failed`, and `internal_error`. Failures before the first
episode attempt use `attempt: 1` without an `attempt_started` record.
JSON display messages are fixed text; raw exceptions, URLs, response bodies,
credentials, and license data are never copied into them.

Authentication details are compatible additions to version 1. In
`phase: "authenticating"` events, `auth_step` identifies the current operation:

| `auth_step` | Exact `display_message` / text-mode message |
| --- | --- |
| `checking_session` | `Checking saved sign-in...` |
| `validating_session` | `Validating sign-in...` |
| `refreshing_session` | `Renewing sign-in...` |
| `saving_session` | `Saving sign-in securely...` |
| `authenticated` | `Signed in successfully.` |

Only operations actually performed are reported. A valid saved cookie normally
produces checking, validating, and authenticated. A missing or known-expired
cookie produces checking, refreshing, validating, saving, and authenticated.
A rejected saved cookie adds validation before refreshing. Validation also
applies to supplied and newly renewed cookies; a supplied cookie skips checking
storage. Saving appears only when persistence is needed, including cookie
rotation. Authentication success is emitted after any required save succeeds.

These steps can occur before `attempt_started` or during an existing attempt
when a bearer token needs refreshing. They never increment the download attempt,
reset track progress, or emit download retry events. After successful bearer
refresh, the previous phase resumes unless another worker has changed it.
`authenticated` describes sign-in success only; it is not a terminal event.

Known authentication failures keep their existing broad `code` and add a
stable `reason`, such as `credential_locked`, `credential_unavailable`,
`bad_credentials`, `session_rejected`, or `session_refresh_unsupported`.
The last reason explains that automatic browser renewal currently requires
Windows and that a fresh cookie must be supplied on Docker/Linux. Unknown
details fall back to the broad code and generic message without a reason.
CVA should use structured fields for behavior and `display_message` for screen
text; neither message wording nor stderr is a parsing contract. Accept missing
or unfamiliar optional fields and retain the existing version-1 validation of
required fields, sequence, terminal records, and exit status.

See the [captured JSONL examples and full reason list](examples/progress/README.md).
The examples are checked against offline CLI runs in the test suite. Only their
elapsed times and temporary output paths are normalized.

Exactly one terminal record is emitted and it is last. The caller must reject
missing terminals, malformed JSON, unsupported versions, broken sequences,
events after a terminal, or a terminal/exit-code mismatch. A killed process or
broken stdout pipe cannot guarantee delivery of a terminal record and must be
treated as incomplete. CVA must independently validate the reported path,
MP4 structure, requested audio/video tracks, and Stage 1/Stage 2 authority.
The downloader checks only that newly muxed output is a nonempty regular file;
`skipped_existing` never implies validation or a newly completed download.

The default `--progress-format text` keeps interactive progress bars and subtitle
handling. Download failures now propagate to a nonzero process exit status in
both modes; text-mode batches still continue after episode failures and report
a nonzero exit status if any episode failed.

### MP4 and external subtitles

Use `--output-format mp4` to mux video and all selected audio tracks into MP4.
Audio and video are copied without re-encoding; their codecs must be supported
by MP4. Requested subtitles and closed captions are saved separately, preserving
the original bytes and format: ASS stays ASS, SRT stays SRT, and VTT stays VTT.
No subtitle tracks are embedded in MP4. Omit the option, or use
`--output-format mkv`, for the existing embedded-subtitle behavior.

Both containers preserve the input packet timestamps during muxing with
FFmpeg's `-copyts`. Independently resetting each input from its reported start
time can introduce a multi-second A/V offset for fragmented AAC whose media
timescale differs from its sample rate. This preserves the source timing without
guessing a fixed delay for an episode or re-encoding its tracks. See
[FFmpeg's timestamp options](https://ffmpeg.org/ffmpeg.html#Advanced-options).

```powershell
.\crunchyroll-downloader.exe --url "https://www.crunchyroll.com/watch/GE00198973JAJP/dawn-and-confusion" --output-format mp4 --audio-lang ja-JP,en-US --subs-lang en-US --cc-lang en-US
```

Sidecars follow [Plex's external subtitle naming convention](https://support.plex.tv/articles/200471133-adding-local-subtitles-to-your-media/):
the exact video basename, an ISO language code, and the source extension.
External filenames omit role indicators such as `forced` and `cc`.
For example (depending on the supplied source tracks):

```text
Example S01E01 - Pilot [1080p].mp4
Example S01E01 - Pilot [1080p].en.srt
Example S01E01 - Pilot [1080p].fr.ass
Example S01E01 - Pilot [1080p].en.vtt
```

For embedded MKV subtitles, `--forced-subs-lang fr-FR` (or a comma-separated
list) can explicitly set the forced disposition on a selected track. It does not
affect MP4 sidecar names, fetch another track, extract signs from full dialogue,
or apply to closed captions.

Regional variants and different subtitle roles can share a Plex language code.
If two selected tracks would have the same filename, the second uses the ISO
639-2/B alias (for example, `.en.srt` and `.eng.srt`). Further tracks go into a
sibling `subtitles` folder, supported
by Plex Media Server 1.41.0+ with the Plex TV Series or Plex Movie agent. The
console reports each source locale and its output path. Files are never silently
overwritten; an existing identical subtitle can be reused, but a differing file
stops the export. Failed exports keep the downloaded inputs and do not leave a
finished-looking MP4. Existing MKV files do not cause MP4 downloads to be skipped.

### Download pacing

If you're getting rate-limited while downloading a season/batch, wait at least this long between each episode:
```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/series/GJ0H7Q5ZJ/hells-paradise --season 1 --etp-rt replace_this --download-delay 30s
```

If Crunchyroll rate-limits an episode anyway, it's retried in place (starting at `-download-delay`, or 1 minute if unset, doubling up to 30 minutes on repeated hits) instead of moving on to the next episode and tripping the same limit again.

## Building

### Requirements

- [Go](https://go.dev/dl/) 1.25 or newer, as declared in [go.mod](go.mod)

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
store. On Linux, they also create a disposable encrypted vault through CVA and
read it through the downloader, checking locked/corrupt/permission failures.
Set `CRDL_TEST_BINARY` to an absolute Linux downloader executable path to include
the complete CLI-to-Python vault-read check. The CI Go suite also runs with the
race detector. With FFmpeg and FFprobe on PATH, the Go tests additionally generate
short synthetic clips to check both containers, copied audio/video packet hashes,
shared packet timing (including 10 MHz AAC media clocks and nonzero video starts),
subtitle byte preservation and naming, and failed-export cleanup. These tests
use no account, network, or downloaded media.

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
