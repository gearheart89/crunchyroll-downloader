# CVA integration handoff: media-window acquisition v1

Date: 2026-11-18. Fork release/tag: **`cva/v1.6.0-3`**.

The downloader side of CVA's 2026-11-18 **revision 2** media-window contract is
implemented. Wire versions remain 1. A live Windows acquisition passed; CVA's
consumer adapter, Debian Docker acceptance and restoration acceptance are still
separate work. This document describes the implemented interface and the work
for the CVA conversation; it does not claim those consumer changes are complete.

## Obtain the tool

Repository: <https://github.com/gearheart89/crunchyroll-downloader>

Release: <https://github.com/gearheart89/crunchyroll-downloader/releases/tag/cva/v1.6.0-3>

Pin this release rather than upstream `1.6.0`, fork `cva/v1.6.0-2`, or a moving
branch. Older releases lack this media-window interface.

| Runtime | Release asset | Suggested installed filename |
| --- | --- | --- |
| Windows x64 | `crdl-windows.exe` | `crunchyroll-downloader.exe` |
| Debian Docker x86-64 | `crdl-linux` | `crunchyroll-downloader` |
| Debian Docker ARM64 | `crdl-linux-arm64` | `crunchyroll-downloader` |

Each asset has an adjacent `<asset>.sha256` checksum file. Verify the downloaded
bytes before installing, retain the pinned tag and hash in the parent build
manifest, and set the executable bit on Linux. macOS assets retain the ordinary
downloader; they do not advertise `media_windows_v1`.

The parent may download these assets during its build/install step without a Go
compiler. The existing intended layout remains:

```text
<application-root>/tools/source/Crunchyroll Downloader/   optional tagged source
<application-root>/tools/Crunchyroll Downloader/          installed executable
```

Keep executable path, process working directory, CVA source/import root, Python
interpreter, and acquisition root configurable. Resolve relative configuration
paths from the parent application's root, then pass resolved paths to the child.
Do not assume the downloader's location determines its working directory.

For a source-build profile, fetch the fork tag and build from its source root:

```sh
git fetch origin tag cva/v1.6.0-3
git checkout --detach cva/v1.6.0-3
go build -trimpath -ldflags="-s -w" -o <configured-destination> .
```

Use an isolated checkout for that recipe. Go requirements/dependencies are in
`go.mod` and `go.sum`; no new Go or Python package dependency was added by this
feature. Include all Go source, `cva_auth.py`, and
`contracts/media-windows-v1/schemas`: the helper and schemas are embedded at build
time. `go run . --capabilities-json` and `go run . --request-json PATH` also work
for source development. Do not copy `.tools`, credentials, device files, media,
or an unrelated local executable into the parent source profile.

Runtime dependencies remain FFmpeg **and FFprobe**, a real Python interpreter
with CVA's declared dependencies, CVA modules, secure credential/session storage,
and the user's configured device material. A frozen CVA executable is not itself
a Python interpreter for `--cva-python`. Runtime acquisition uses an H.264 decoder;
only synthetic test generation needs the `libx264` encoder.

Device discovery still uses the child's **working directory**: a filename ending
in lowercase `.wvd`, or the exact pair `client_id.bin` and `private_key.pem`.
Configure a controlled directory containing the intended device files. There is
no new device-path argument in this interface.

## Contract and implementation

The authoritative bundle is [contracts/media-windows-v1](contracts/media-windows-v1/README.md),
imported from `cva-media-windows-contract-v1-20261118-r2.zip`. Its schemas and
examples are unchanged; `SHA256SUMS` verifies the original bundle. The bundle's
original design-status wording is historical; this handoff and the README
describe implementation status. Its examples are synthetic, not live receipts.

Implemented behavior:

- An offline capability query and a separate request-file acquisition entry point.
- One exact episode presentation/version per request; no silent locale or
  representation substitution. `highest` selects once and remains pinned.
- Static DASH SegmentTemplate timelines/durations and flat SegmentBase/SIDX
  byte ranges. Only selected dependency-expanded video portions are fetched;
  there is no whole-episode fallback.
- Overlap coalescing and temporary byte reuse within one invocation. Several
  windows may reference one clip; a split window may reference several clips.
- Self-contained MP4s with copied progressive 8-bit H.264 4:2:0 samples; no donor
  audio/subtitles, encoder, resizing, image filtering, or color conversion.
- Per-sample timing/hash JSONL, representation metadata, immutable result
  directories, typed failures, partial results, and cancellation handling.
- IDR/dependency checks, copied-sample/configuration/timing checks, and full
  delivered-clip decoding with network access disabled for the media tools.
- Existing platform-aware CVA credentials, stored-session reuse and renewal.

The main code is in `windows_contract.go`, `windows_cli.go`,
`windows_manifest.go`, `windows_acquire.go`, `windows_media.go` and the platform
publication files. Despite the filename prefix, this is **media windows**, not
a Windows-only implementation. Runtime targets are Windows and Linux amd64/arm64.
Ordinary MKV/MP4 downloads and `cva-downloader-progress` v1 remain separate.

## Invocation and authentication

First invoke only `--capabilities-json`. Require schema
`cva.downloader-capabilities`, version 1, and an `operations` entry with
`name: acquire_windows`, `capability: media_windows_v1`, and version 1 in its
request/result/sample-map/progress version lists. It is offline and exits zero.
Missing support means unsupported; do not substitute an ordinary full download.

For the current local Windows checkout:

```powershell
Set-Location 'C:\CODEX\CuteTenshii - crunchyroll-downloader'
.\crunchyroll-downloader.exe --capabilities-json
.\crunchyroll-downloader.exe --request-json 'C:\CVA Work\requests\minute.json' --cva-root 'C:\CODEX\Crunchyroll Video Analyzer' --cva-python 'C:\path\to\CVA-python.exe'
```

For a configured Debian container (paths are illustrative):

```sh
cd /run/cva/downloader-device
'/opt/cva/tools/Crunchyroll Downloader/crunchyroll-downloader' --capabilities-json
'/opt/cva/tools/Crunchyroll Downloader/crunchyroll-downloader' \
  --request-json /work/requests/minute.json \
  --cva-root /opt/cva \
  --cva-python /opt/cva-venv/bin/python
```

The parent should spawn an argument list with `shell=False`, not concatenate a
shell command. Paths with spaces remain one argument; do not embed shell quotes
inside that argument. Drain stdout and stderr concurrently. The optional
`CRUNCHYROLL_CVA_ROOT` and `CRUNCHYROLL_CVA_PYTHON` environment variables remain
supported. Explicit paths are preferable to sibling-folder autodetection when
the parent installs the tool under `tools`.

The saved session is tried first. Missing/expired/rejected sessions use the
existing Windows browser flow or CVA's owned container-authentication handoff;
successful replacements are validated before secure persistence. Windows uses
the same user context as CVA. Containers retain CVA's selected vault, separate
unlock key and ownership/permission requirements. No credentials belong in the
request, logs, executable arguments, or result artifacts.

**Container integration requirement:** construct/authorize the existing
`ContainerAuthenticationHandoff` for exactly
`https://www.crunchyroll.com/watch/<watch_id>` with no title slug or query, and
pass its invocation-scoped `CVA_DOWNLOADER_AUTH_SOCKET` environment to the child.
Use the request's `watch_id`, not its `presentation_id`. The downloader passes
that canonical string to `container_auth.request_session_refresh`, and CVA checks
exact source equality. Keep the handoff alive through child exit and tie it to
job cancellation. A valid cached session can work without renewal, so test an
expired-session case separately to establish that the socket is wired correctly.

Do not combine `--request-json` with `--url`, `--file`, `--output-dir`,
`--filename`, `--output-format`, language/quality flags, `--etp-rt`,
`--progress-format`, or debug mode. Only `--cva-root` and `--cva-python` are
additional options for this mode. Credential checks are separate invocations.

## Example request

This is a repeatable first-minute request shape using the presentation tested on
Windows. CVA must create the private parent directory, choose a fresh acquisition
directory, and write UTF-8 JSON without a BOM. Do not create the final acquisition
directory in advance. Replace request ID/output path for each new job.

```json
{
  "schema": "cva.media-window-request",
  "version": 1,
  "request_id": "cva-minute-0001",
  "operation": "acquire_windows",
  "source": {
    "provider": "crunchyroll",
    "watch_id": "GMKUXQ2VM",
    "presentation_id": "GMKUXQ2VM",
    "presentation_locale": "ja-JP",
    "expected_snapshot_id": null
  },
  "video": {
    "selection": "highest",
    "representation_id": null,
    "allow_representation_substitution": false
  },
  "streams": {"video": true, "audio": false, "subtitles": false},
  "windows": [{
    "window_id": "first-minute",
    "anchor": "episode_start",
    "start_seconds": "0/1",
    "end_seconds": "60/1",
    "context_before_seconds": "0/1",
    "context_after_seconds": "0/1",
    "boundary_policy": "require_requested_interval"
  }],
  "delivery": {
    "container": "mp4",
    "self_contained": true,
    "video_encoding": "forbidden",
    "timestamp_policy": "rebase_with_exact_mapping",
    "dependency_padding": "expand_to_decodable",
    "max_dependency_padding_before_seconds": "30/1",
    "max_dependency_padding_after_seconds": "30/1",
    "allow_discontinuity_split": true
  },
  "output": {
    "directory": "C:\\CVA Work\\acquisitions\\minute-0001",
    "overwrite": false
  }
}
```

For Docker, replace `output.directory` with a native container path such as
`/work/acquisitions/minute-0001`. It must be absolute, new, and beneath an existing
private parent. Resolve `..` before constructing it. The request-file argument
itself may be relative to the child's working directory. The ordinary download
flags' more permissive output-path behavior does not apply to this JSON field.

All intervals are half-open. Rational strings must be reduced, with positive
denominators and no negative zero. Use integers/rational arithmetic, not floats.
Validate presentation IDs against episode versions metadata; the example's equal
watch/version IDs are not a general rule.

For first/last five-minute discovery, use
[request-discovery.json](contracts/media-windows-v1/examples/request-discovery.json):
`episode_start [0,300)` and `episode_end [-300,0)`, with `clamp_to_episode`.
For narrow follow-ups, use
[request-narrow.json](contracts/media-windows-v1/examples/request-narrow.json),
`require_requested_interval`, the previous `representation.id`, and
`source.snapshot_id` as `expected_snapshot_id`. Representation IDs and snapshots
are opaque. A snapshot binds source/init/timing/index metadata, not proof that
media bytes can never change. Clip/sample hashes remain the integrity evidence.

## Consume progress and published results

Create a dedicated parser for **`cva-media-window-progress` version 1**. Do not
send these messages to CVA's existing singular video/audio download validator.
Use the bundled schemas, allowing at most 65,536 UTF-8 bytes per JSONL record
including newline. Sequences begin at 1 and remain contiguous. Request/attempt
IDs remain fixed; attempt IDs are generated by the downloader.

Event types are `phase`, `window_progress`, `warning`, and `terminal`. Transfer
counters are per window, use `segments`, have a frozen positive total, never
regress, and use integer `floor(100 * completed / total)`. Phases can repeat and
do not reset transfer counters. Byte ranges count as segments too. A transfer at
100% still has assembly, validation and publication ahead of it.

Require exactly one terminal and no data after it. Accept complete acquisition
only when exit code is 0, terminal/result both say `completed`, IDs match, the
raw `result.json` SHA-256 matches the terminal, and all referenced artifacts and
coverage validate. Hash original file bytes, not reserialized JSON.

Partial and failed acquisitions exit 1. A `partial` result can retain usable
independently validated clips; do not discard that distinction or automatically
treat it as complete restoration. Early failure may have `result: null` and a
null `request_id`. A crash, missing terminal, malformed stream, ID/sequence error,
or status/hash/exit disagreement is incomplete. Use typed codes from the schemas,
not English message matching. Drain and bound diagnostics separately.

The successful live run's actual terminal was:

```json
{"attempt_id":"55a8399b-810d-4f1b-af74-e9bf539e4ee6","error_code":null,"event":"terminal","message":"Requested media windows acquired.","protocol":"cva-media-window-progress","request_id":"live-minute-a39d04bd64ab-04","result":{"path":"result.json","sha256":"cf1ae90ed6de79613ba79d1e5a81f608fed5cbb691069da098e1bfa411bdad80"},"sequence":31,"status":"completed","version":1}
```

That is historical evidence, not a reusable receipt: its media/result files
were deleted. More synthetic completed/partial/invalid examples are in the
contract bundle. The successful run's stderr was exactly:

```text
Checking saved sign-in...
Validating sign-in...
Signed in successfully.
```

After publication, `output.directory` contains `result.json`, one or more MP4s,
and one `*.timeline.jsonl` per clip. Resolve only safe relative artifact paths
under that directory; reject traversal, symlinks/reparse points and unexpected
types. Check schemas, hashes, sizes, stream identity, shared representation and
each window's `clip_ids`/resolved coverage. Directories are immutable and never
overwritten. Retry in a new directory/attempt; there is no cross-process partial
download resume. Reuse must validate identities, hashes and coverage first.

## Timing and downstream restoration

Each sidecar row describes one encoded sample in **decode order**. PTS can reorder;
integer ticks are strings. Use `source_time_base` and `clip_time_base` from the
clip, and `episode_time_seconds` from each row to map clip pictures back to the
selected original presentation. Never assume the clip starts at episode zero,
that source and clip clocks are identical, or that frame index implies time.

Raw source PTS/DTS are retained. Single normal-speed edit-list offsets are
applied before DASH period/PTO mapping. Final clip PTS/DTS are measured after
remuxing; constant shifts can occur and are recorded rather than guessed.
Internal coverage checking permits one source-clock tick of rounding where
reordered pictures meet, requires continuous decode samples, and does not alter
timestamps, extend outer coverage, or bridge larger gaps.

`source_sample_sha256` hashes decrypted, length-prefixed compressed AVC sample
bytes; it does not hash encrypted network data. This implementation preserves
those bytes, reports `video_reencoded: false`, and reports no bitstream filters.
Codec-configuration hashes cover the `avcC` payload, excluding its box header.

Respect `requested_content`, `analysis_context`, and `decode_dependency` roles.
Extra decoding samples are not automatically approved replacement material.
Unknown metadata stays null (including currently `average_frame_rate`); color
values such as `tv` are reported FFprobe labels. Do not invent values or enforce
the synthetic examples' metadata as if it were the only valid representation.

CVA still owns frame matching, clean-donor evidence, masks, scene-stable color
transforms, review/approval, and rendering from immutable originals. Recipients
retain their audio/subtitle payloads and timing. Acquisition does not prove
matching animation, clean subtitles, or lossless final compositing. The existing
180-second analysis cap can be handled by bounded inspection subwindows over
five-minute acquisitions. Do not silently turn on restoration or smart rendering.

## Acceptance evidence and remaining work

Windows live test, 2026-11-18 (America/Chicago; receipt UTC 2026-11-19):

| Check | Result |
| --- | --- |
| Episode/version | The Apothecary Diaries S01E01, Maomao; `GMKUXQ2VM`, `ja-JP` |
| Authentication | Windows credential-store readability passed; saved session reused; no Chrome launch |
| Requested interval | `[0,60)` seconds, no analysis context |
| Delivered interval | `[0,3003/50)` = 60.06 seconds, including decoding padding |
| Video | One 1920x1080 H.264 High, 8-bit yuv420p stream; 1,440 decoded frames |
| Audio/subtitles | None, as required for donor acquisition |
| Transfer | 23 dependency-planned segments of 343 indexed segments; no full-episode acquisition |
| Size | 49,800,417 bytes |
| Integrity | File/sidecar hashes and all 1,440 encoded-sample hashes matched |
| Timing | Packet PTS/DTS/durations, edit-derived episode mapping and coverage passed |
| Media | Full offline FFmpeg decode passed; no re-encoding |
| Protocol | 31 schema-valid progress records, completed terminal, matching result hash, exit 0 |
| Cleanup | Test media, sidecars, requests, logs and temporary diagnostic executables/source deleted |

The live test found and fixed missing provider-manifest authentication/client
headers, playback cleanup headers, single-edit timeline handling and one-tick
reordered-frame coverage rounding. Bearers are restricted to the exact HTTPS
provider origin; CDN requests receive neither bearer nor cookie credentials.

Verification also included the full Windows Go suite with required media tools,
`go vet`, formatting/diff checks, synthetic media-window tests under native Linux
via WSL, and focused Linux retests of the live-test fixes. Cases include B frames,
open GOP rejection, VFR, nonzero/negative origins, encrypted fixture samples,
SIDX ranges, overlap, boundary policies, configuration changes, partial failures,
cancellation, immutable publication and authentication/redirect boundaries.
The Python authentication suite ran 37 tests with 6 explicit skips: native Linux
IPC tests and optional local-CVA contract tests were not enabled in that Windows
run. The live saved-session test does not validate expired-session renewal.

The pre-release Windows binary used for the live test had SHA-256
`94f11c3d5db329c50e218884805eec9d926042a6a7a38c848d5ba8c30f5f07ee`.
Release builds have their own adjacent checksum files; do not expect this
working-tree build hash or its `-modified` version string to identify a release.

CVA integration tasks, in order:

1. Pin/download the release asset per OS/architecture (or build the pinned tag),
   verify its checksum, and run the capability check from the configured path.
2. Add a separate media-window request builder, JSONL parser and result validator.
   Use the bundled schemas; retain the legacy ordinary-download adapter.
3. Wire absolute private acquisition paths, process CWD/device lookup, Python/CVA
   paths, and the existing container handoff with the canonical watch URL.
4. Carry exact sample/clip/episode mappings and roles into analysis and reference
   storage; handle shared/split clips, partial results and immutable retries.
5. Test the CVA adapter against completed/partial/failed/malformed fixtures, then
   run the one-minute acceptance case through CVA itself on Windows and Debian
   Docker. Include stored-session reuse, forced expiry/renewal, cancellation,
   spaces/different CWD, and safe cleanup. ARM64 needs native acceptance too.
6. Separately qualify opening/ending discovery, pinned narrow follow-ups and
   restoration/rendering. Windows live testing covered one opening minute, not
   every source format or a live end-relative/five-minute acquisition.

Unsupported cases return typed errors: other codecs/HDR/interlacing, nested
indexes, complex edits, unprovable dependencies, and representation/configuration
changes. Limits include 1 MiB requests, 16 MiB manifest metadata, 128 MiB per
segment, 512 MiB per coalesced encoded span, 128 groups/clips, 90-second media
HTTP timeouts and a two-hour invocation limit. A capability advertises an
implemented operation, not universal source compatibility or accepted rendering.
