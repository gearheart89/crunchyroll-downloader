# Maintaining this fork

This is an independently maintained fork for use by CVA and other parent
applications. Upstream is a source of fixes and features to evaluate. Sending
changes upstream, publishing releases, and adopting every upstream release are
optional.

Use this guide when an upstream release is worth reviewing, when a service
change breaks the downloader, or when updating CVA, dependencies, or packaging.
It does not require a schedule or an update while the current version is working.

## Starting point

Recorded on **2026-10-28**; recheck these values before future integration work.

| Item | Recorded value |
| --- | --- |
| Fork (`origin`) | `https://github.com/gearheart89/crunchyroll-downloader.git` |
| Original project (`upstream`) | `https://github.com/CuteTenshii/crunchyroll-downloader.git` |
| Last fully integrated upstream release | `1.6.0` |
| Upstream baseline commit | `182ea1b84bbfcdd5755fa8cd4d07a901fea1c199` |
| Current customization branch | `feature/cva-keyring-auth` |
| Fork checkpoint | `cva/v1.6.0-1` |

This checkpoint includes output-path options, subtitle naming, JSON progress,
session caching, authentication details, the audio/video timestamp fix, and
their tests and documentation. Resolve its exact commit with
`git rev-parse "cva/v1.6.0-1^{commit}"`. Local executables are ignored by Git;
rebuild from the tagged source or use the matching fork release assets.

Before applying a future update, preserve the complete customized source in a
local checkpoint. A branch created from HEAD alone does not preserve uncommitted
or untracked files. Publishing that checkpoint remains a separate action.

## Fork tag names

Use annotated tags named `cva/v<upstream-version>-<fork-revision>`. The `cva/`
namespace separates our tags from upstream's ordinary version tags:

- `cva/v1.6.0-1`: first tagged CVA snapshot based on upstream 1.6.0.
- `cva/v1.6.0-2`: another fork update on the same upstream baseline.
- `cva/v1.7.0-1`: first fork snapshot after fully integrating upstream 1.7.0.

These are Git tag names, not Semantic Versioning prerelease identifiers. Keep
the upstream version unchanged for selective ports and record those source
commits separately. Never move an existing release tag to different code;
increment the fork revision. Commit the complete source before tagging it.
Push explicitly to `origin`, and publish releases only in the fork. The current
release workflow builds Windows amd64, Linux amd64, and macOS amd64/arm64 assets
when a GitHub release is published; creating a local tag does not run it.

## Behavior to preserve

The [README](README.md) and [JSON examples](examples/progress/README.md) describe
the current public interface. Treat the following as the compatibility checklist
until a deliberate, coordinated change replaces it.

| Area | Required behavior | Main implementation and regression coverage |
| --- | --- | --- |
| CVA credentials | Use CVA's platform selector and storage factory, including configuration overrides. Windows keyring and Docker encrypted vault remain supported. Preserve the original account/password store and sanitized locked/corrupt/unavailable failures. | [Bridge](cva_auth.py), [Go adapter](cva_auth.go), [Python tests](tests/test_cva_auth.py), [adapter tests](cva_auth_test.go) |
| Session cookies | Try the stored cookie first. Renew only when missing, known-expired, or explicitly rejected. Validate replacements before saving; temporary network/service failures do not trigger renewal or overwrite the cache. Preserve device identity and cookie rotation. | [Session provider](session_auth.go), [token exchange](token.go), [session tests](session_auth_test.go) |
| Authentication concurrency | Use the selected provider for initial sign-in and bearer refresh. Concurrent workers share refreshes; retrying a request preserves its body. Cookie renewal does not become another episode attempt. | [HTTP requests](http_request.go), [HTTP tests](http_request_test.go), [authentication progress tests](auth_progress_test.go) |
| Output containers and subtitles | MKV remains the default. MP4 copies audio/video without re-encoding and exports requested subtitles separately in their original format and bytes. Sidecars use the final video basename plus language; omit `.forced` and `.cc` filename markers. Preserve collision handling and existing-file protection. JSON mode leaves subtitles to CVA. | [Output](output.go), [output tests](output_test.go) |
| Paths and filenames | `--output-dir` accepts native absolute/relative paths, spaces, Unicode, and `..`, creates the destination, and does not add a series directory when supplied. Relative paths resolve from the process working directory. `--filename` is a literal basename for one episode, not a template or directory. | [Path handling](output_paths.go), [path tests](output_paths_test.go) |
| JSON lifecycle | Preserve protocol v1, clean JSONL stdout, sequence numbers, track identity/progress rules, safe messages, and terminal/exit-code agreement. Keep optional authentication steps/reasons compatible. Text diagnostics stay separate. | [Reporter](progress_json.go), [authentication messages](auth_progress.go), [protocol tests](progress_json_test.go), [captured examples](examples/progress/README.md) |
| Download reliability | Preserve bounded segment processing, ordered writes, propagated worker/write failures, rate-limit backoff, request refresh behavior, and cleanup reporting. Do not restore success exits after a failed download or mux. | [Download tests](download_test.go), [backoff tests](backoff_test.go), [manifest tests](ondemand_test.go), [DRM metadata tests](drm_test.go) |

Important details behind that checklist:

- The session keyring service is `Crunchyroll Downloader/Crunchyroll Session`.
  The container session vault is `<CVA_CREDENTIAL_VAULT_PATH>.downloader-session`
  and uses CVA's configured unlock key. Neither replaces CVA's account/password
  record. There is no plaintext session fallback.
- An explicit `--etp-rt` without `--save-cookie` remains a one-run override that
  does not need CVA/Python. With `--save-cookie`, validate before persisting it.
  Rejected explicit input must not silently switch to another account.
- A valid saved cookie works on Linux/Docker. Automatic browser renewal currently
  requires Windows. Preserve the actionable `session_refresh_unsupported` failure
  until Linux renewal is intentionally implemented and tested.
- JSON mode accepts one `/watch/` episode and produces video/audio only.
  Requested audio must be available; duplicate requested locales download once.
  The parent owns subtitle handling and independent output validation.
- Preserve the shared audio/video packet timeline when muxing both containers
  (`-copyts`). Do not independently reset streams from reported format start
  times or hard-code an episode delay. [Timing regression tests](output_timing_test.go)
  cover fragmented AAC with a 10 MHz clock, ordinary AAC, B-frames, nonzero video
  starts, and MKV subtitle timing while checking copied packet payloads.
- `authenticated` means sign-in succeeded, and track progress of 100 means a
  transfer finished. Neither means the invocation completed. Completion follows
  output publication, joined workers, and cleanup; `skipped_existing` means only
  that the output path already exists. Partial-download resume is not implemented.
- Parallel workers can change phases during bearer refresh. Restore the latest
  work phase without resetting track progress. Preserve broad failure codes;
  unknown optional authentication details must not expose raw helper data.
- Keep cookie/token/password values, request headers, license material, raw
  response bodies, and raw exceptions out of JSON display messages and examples.
  Test using synthetic values and local HTTP fixtures.

## Reviewing a future upstream candidate

The commands below are instructions for a future review, not an automated
updater. Replace uppercase placeholders with verified refs/SHAs before use.

1. Inventory the current state with `git status --short`, `git branch
   --show-current`, `git rev-parse HEAD`, and `git remote -v`. Inspect tracked
   diffs and untracked source files. Record the baseline and relevant CVA version
   or source snapshot; CVA's internal Python APIs are a separate dependency.
2. Fetch for inspection without merging. Fetch the chosen upstream tag into a
   separate ref namespace so it does not replace the fork's release tags:

   ```sh
   git fetch --no-tags upstream
   git fetch --no-tags upstream refs/tags/VERSION:refs/upstream-tags/VERSION
   git rev-parse "refs/upstream-tags/VERSION^{commit}"
   ```

   Record the resulting commit SHA. Fetch updates repository refs/objects but
   does not integrate changes into the working files. Do not force-replace a
   previously recorded candidate if the upstream tag has moved; investigate the
   changed identity. See the [Git fetch reference](https://git-scm.com/docs/git-fetch).
3. Compare the upstream baseline with that candidate, separately from our fork:

   ```sh
   git log --oneline UPSTREAM_BASE_SHA..CANDIDATE_SHA
   git diff --stat UPSTREAM_BASE_SHA CANDIDATE_SHA
   git diff UPSTREAM_BASE_SHA CANDIDATE_SHA
   ```

   Read release notes and actual code. Identify useful fixes, dependencies,
   interface changes, and overlap with the checklist above. Also inspect existing
   fork changes; a committed-only comparison is incomplete while work is dirty.
4. Choose **defer**, **selective port**, or **full merge**. Explain why the useful
   changes justify their integration risk. A selective port must account for
   prerequisite commits and behavior changes, not only the visibly failing line.

## Integrating a candidate

Start from a complete, clean local checkpoint of the customized source once
checkpointing is authorized. Create a separate review branch from that exact
customized revision, not from upstream or an older default branch:

```sh
git switch -c review/upstream-VERSION CUSTOM_BASE_SHA
```

For a full merge, retain a review point before committing:

```sh
git merge --no-commit --no-ff CANDIDATE_SHA
```

`--no-ff` also prevents a fast-forward from bypassing that review point. Resolve
conflicts by preserving intended behavior on both sides; do not accept every
conflict as "ours" or "theirs." A clean merge still requires testing. If abandoning
an active merge, use `git merge --abort` on this clean-start review branch.
See the [Git merge reference](https://git-scm.com/docs/git-merge).

For selected ordinary commits, apply prerequisites in order and inspect each
result; `git cherry-pick --no-commit COMMIT_SHA` permits review before committing.
Record source SHAs when the eventual fork commit is made. Merge commits require
separate ancestry analysis. If a cherry-pick is stopped by a conflict, use
`git cherry-pick --abort` to abandon that active operation. It is not a generic
undo command after successful uncommitted application. See the
[Git cherry-pick reference](https://git-scm.com/docs/git-cherry-pick).

Keep the maintained branch and last accepted executable/source package available
while reviewing. If rejecting a successfully applied candidate, preserve anything
worth keeping and retire the disposable candidate work; do not hard-reset the
maintained branch. Commit, move accepted changes into the maintained branch, and
publish only within the owner's instructions at that time.

## Validation before accepting an update

Run checks in the candidate source directory. Use Go from [go.mod](go.mod), the
Python interpreter intended for CVA, and FFmpeg/FFprobe on PATH. Format changed Go
files with `gofmt`; avoid rewriting unrelated source or local tool caches.

```sh
git diff --check
go vet ./...
go test -count=1 -timeout 5m ./...
python -B -m unittest discover -s tests -v
```

Use `python3` on Linux when appropriate. For concurrency changes, also run the
CI-style race check on a host with the Go race detector's C toolchain available:

```sh
go test -race -count=2 -timeout 5m ./...
```

| Check | Acceptance evidence |
| --- | --- |
| Windows and Linux execution | Run the relevant suites on both platforms. Cross-compilation alone does not validate paths, Python invocation, or vault permissions. Identify whether Linux evidence came from WSL, Debian Docker, or another environment. |
| Real CVA API compatibility | Set `CVA_TEST_ROOT` to the updated CVA source and install its own declared requirements in the test interpreter. Python contract tests use fake keyring/browser implementations and disposable vaults. On native Linux, cover the real encrypted-vault implementation. |
| Optional CLI/vault integration | When a separate candidate executable is available, set `CRDL_TEST_BINARY` to its absolute Linux path for the fixture credential-read subprocess check. This is not a live account sign-in test. |
| Media and paths | FFmpeg/FFprobe fixtures verify MKV/MP4, copied packet data, original subtitle bytes/names, relative output paths, and failed-export cleanup. Without these tools, media tests skip; report that gap. |
| Progress/parser contract | `TestJSONCLIAuthenticationExamples` checks the captured examples against actual offline CLI output. Keep `CRDL_UPDATE_PROGRESS_EXAMPLES` unset during verification. Change fixtures only after intentionally reviewing a contract change. |
| Parent integration | Exercise configured executable/CVA paths, spaces, absolute overrides, a different launch directory, and readable JSON stdout with separate stderr. Confirm the parent validates terminal/exit status and output independently. |

Missing Python, an unset `CVA_TEST_ROOT`, or platform restrictions can skip checks.
Report skips and unresolved failures explicitly. Offline tests do not establish
live service compatibility. If a live check is needed, use the agreed account and
scope; an authentication-only check does not prove the media download path.

Changes to CVA storage APIs, Go dependencies, Python pins, FFmpeg behavior, or the
container base image need their own compatibility review. Do not broaden an
upstream fix into unrelated dependency upgrades. Prefer the existing manifests
over a second manually maintained dependency list.

## Passing an accepted update to the parent project

The intended parent layout is:

```text
<application-root>/tools/source/Crunchyroll Downloader/   # source
<application-root>/tools/Crunchyroll Downloader/          # built tool
```

Windows uses `crunchyroll-downloader.exe`; Linux uses `crunchyroll-downloader`.
The parent owns its configurable executable paths and build profiles. Relative
configuration paths resolve from its application root, while the downloader's
relative `--output-dir` resolves from its configured process working directory.
Widevine device-file discovery currently also uses that working directory.

Include Go source, `go.mod`, `go.sum`, the embedded `cva_auth.py`, license, and
documentation/examples when handing off source. Exclude `.git`, `.tools`, caches,
credentials, device secrets, and media. Record the exact fork revision, or the
fact that a supplied source snapshot contains uncommitted changes. A source copy
does not update an existing executable; rebuild through the parent profile after
acceptance. Source/dev, packaged Windows, and Debian Docker profiles can include
different build assets while retaining the same runtime contract.

The runtime still needs CVA modules and its Python dependencies, FFmpeg, and the
configured credential storage. `cva_auth.py` is embedded, but CVA itself is not.
In a frozen parent application, configure a real Python runtime rather than
assuming the parent executable can interpret Python. Docker must persist the
writable vault directory and keep its unlock key separately mounted with CVA's
required ownership/permissions. Keep credentials out of images and source copies.

## Review record

Append a dated entry here for each future candidate, including deferred reviews:

```text
Review date:
Upstream release and exact candidate SHA:
Previous fully integrated upstream baseline:
Custom fork checkpoint/source snapshot:
CVA revision or source snapshot:
Decision: defer / selective port / full merge
Selected source commits and prerequisites (if any):
Behavior changes and conflict resolutions:
Windows checks, tool versions, and results:
Linux checks, actual environment, tool versions, and results:
Skipped checks, live-check scope, and remaining limitations:
Parent/parser/configuration impact:
Accepted fork revision and retained rollback source/artifact:
Distribution status: local source / local binary / published release
```

Update the fully integrated upstream baseline only after accepting the complete
upstream change set. Selective ports should list their source commits without
claiming that the entire candidate release was integrated. Tagging or publishing
is independent of maintaining this record.
