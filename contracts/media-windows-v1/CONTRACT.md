# CVA opening/ending media-window contract v1

Date: 2026-11-18. Handoff revision: 2. Status: design contract for downloader implementation.
This revision supersedes the initial 2026-11-18 handoff; wire version remains 1
because no deployed implementation of this interface has been accepted.
This is not a declaration of implemented downloader/CVA interoperability or
completed restoration acceptance. No acquisition or rendering is authorized by
the existence of this document alone.

## Purpose and ownership

Acquire immutable, traceable source windows for optional opening/ending
burned-in subtitle restoration. The downloader acquires and assembles encoded
video without re-encoding. CVA establishes exact frame correspondence, confirms
clean donor coverage, produces masks, obtains approval and composites recipients.
Neither source acquisition nor a successful render establishes clean restoration.

Recipients keep their own video outside approved masks, audio, existing
subtitles, presentation timestamps, durations and synchronization. Different
intervals or disjoint regions may use different donors. Restoration can be
disabled; ordinary selected recipient imagery, including burned-in text, then
remains in use. Existing timeline reconciliation is a separate operation.

The intended color correction is stable per scene/shot, not independently
estimated for every output frame. SDR is the initial target; missing metadata
must not be invented and an unsupported transfer/bit depth must be reported
rather than silently converted. This proposal does not establish that every
Crunchyroll source is SDR.

## Delivery and source identity

1. Deliver independently readable, video-only MP4 files, one per continuous
   compatible timeline span. Each file includes its own initialization/codec
   configuration. Initialization plus loose fragments is not the v1 interface.
2. One request addresses one exact episode, presentation/version and selected
   representation. Do not substitute languages, versions, resolutions or codec
   configurations silently. `highest` resolves once, then remains pinned during
   the request. A narrower follow-up should use that explicit representation ID
   and the expected source snapshot.
3. V1 has one shared representation/configuration descriptor. Every delivered
   clip must match it. A codec-configuration change returns
   `codec_configuration_changed`; changes to other shared representation properties
   return `representation_changed`. Do not deliver mixed configurations under the
   shared descriptor, silently transcode, or claim complete coverage. Already
   acquired compatible clips may form an explicit partial result.
   A window may reference multiple clips across periods/discontinuities only
   while that shared descriptor remains accurate. Split incompatible timeline
   spans instead of concatenating them; each clip has one video track. A future
   protocol version may introduce per-clip configuration descriptors.
4. Preserve encoded video samples. No encoder, resize, color correction, frame
   duplication, frame-rate conversion or image filtering is part of acquisition.
   Lossless container/framing changes are permitted and any bitstream filters
   must be reported. Sample hashes describe actual bytes at each side of such
   a transformation; equality must not be asserted if framing changed.
5. Exclude audio and subtitle acquisition for this donor operation. This is a
   video-only v1 contract, not an extension of the recipient's media stream set.
6. SHA-256 hashes bind the actual delivered files and timeline sidecars. Source
   snapshot IDs must be opaque, documented and stable across mere signed-URL
   refresh. They identify the resolved snapshot, not proven media equality across
   every future acquisition. Provider revision IDs can be null when unavailable.
7. Report unknown color/pixel metadata as null. Exact codec configuration and
   dimensions must be available; missing critical timing or dependency evidence
   is a failure, not a guessed value.

## Request boundaries and decoding dependencies

All intervals are half-open `[start, end)`. Rational seconds use strings such as
`120/1` or `1001/24000`, in lowest terms with positive denominators and no `-0`.
Integer ticks are decimal strings, preserving values beyond JavaScript's exact
integer range. Time bases are positive rational seconds per tick.

`episode_start` offsets refer to the selected presentation's visible time zero
and are nonnegative. `episode_end` offsets are nonpositive, relative to that exact
presentation's media end. Resolve end-relative requests from media timing, not
an approximate catalog duration. Each window explicitly declares `boundary_policy`:

- `clamp_to_episode`: intersect requested content with the available visible
  episode interval. Intended for discovery; report the shortened resolved bounds.
- `require_requested_interval`: the full requested content must exist within the
  visible episode interval. Reject an out-of-bounds content request rather than
  shortening it. Intended for precise acquisition.

Both policies clamp optional analysis context at true episode endpoints, reject
reversed/empty intervals and reject requested content wholly outside the episode.
Neither policy permits silently skipping an internal gap or using unrelated
footage. The policy is explicit; never infer it from window names or request order.
Reversed/empty intervals are `invalid_request`. Unavailable requested coverage,
including an out-of-bounds strict request, is `coverage_incomplete`; it must not
be reported as a successful shortened precise request.

First acquisition may request the first and last 300 seconds. Coalesce overlapping
or duplicate acquisition and let several window IDs reference the same clip.
Narrow follow-ups use identified opening/ending intervals. The downloader does
not detect what an opening or ending is.

Three different boundaries must remain distinguishable:

- Requested content: the interval CVA asked to inspect/use.
- Analysis context: optional surrounding footage requested for comparison/review.
- Decode dependencies: additional source samples needed for independent decoding.

For example, content `[120,150)` with five seconds of context asks for visible
coverage `[115,155)`. Actual delivery might cover `[113,157)` to satisfy frame
dependencies. Padding limits apply to dependency expansion beyond the resolved
context, not to the separately requested context. At true episode endpoints,
unavailable context can be clamped. Internal period boundaries require delivery
across compatible split clips, or a typed error when splitting is disabled or
impossible. Missing dependencies cannot be treated as harmless.

Retrieve sufficient preceding random-access/recovery information and trailing
reference data. A packet's keyframe flag alone is not proof that an open GOP is
self-contained. If dependency closure cannot be established, or exceeds an
explicit limit, return a typed error. Do not synthesize frames or encode around
the boundary to conceal missing dependencies.

Extra decoded material remains identifiable in the clip and mapping. Hidden
edit-list trimming must not be the only record of omitted/extra material.
Negative DTS, reordered PTS, and source nonzero origins must be described rather
than discarded. Leading decode-only material can map before visible time zero;
it must never be substituted into a recipient based solely on that timestamp.

## Schemas, examples and filesystem contract

Formal JSON Schema draft 2020-12 files:

- [Request](schemas/request.schema.json)
- [Result](schemas/result.schema.json)
- [One encoded-sample JSONL row](schemas/sample-map.schema.json)
- [Capability query response](schemas/capabilities.schema.json)
- [One separately versioned progress JSONL event](schemas/progress.schema.json)

Examples are synthetic, contain no media or credentials, and are not receipts
for existing MP4s. Repeated-digit hashes are syntactically valid placeholders.

- [Initial first/last-five-minute request](examples/request-discovery.json)
- [Narrow follow-up request](examples/request-narrow.json)
- [Completed result](examples/result-completed.json)
- [Partial result](examples/result-partial.json)
- [Failed result](examples/result-failed.json)
- [One illustrative sample mapping row](examples/sample-map-row.jsonl)
- [Capability response](examples/capabilities.json)
- [Completed progress stream](examples/progress-completed.jsonl)
- [Partial terminal event](examples/progress-partial.jsonl)
- [Invalid-request terminal without a result file](examples/progress-invalid-request.jsonl)

The narrow request pins `example-video-representation` and `example-snapshot-1`,
matching the illustrative resolved identities in the result. Discovery uses
`highest` with no expected snapshot. Production follow-ups must pin the actual
identities obtained by their own discovery; example IDs are not real resources.

All declared fields are required unless the schema's required list says otherwise.
Nullable fields require explicit null when unknown. Unknown fields/versions must
not be silently accepted as this version. Identifier lengths, array bounds and
safe relative result paths are constrained by the schemas.
All schema references are internal to their containing file; the schemas can be
used offline by a compatible validator without fetching another contract file.

CVA supplies an absolute authorized private output directory using native
platform syntax. Result artifact paths are portable, case-sensitive, forward-
slash relative paths under that directory. Reject absolute paths, traversal,
drive/stream components, symlinks/reparse points and unexpected file types.
Do not overwrite an existing acquisition. Requests/results contain paths and
public/opaque identities, never passwords, cookies, signed resource URLs, keys,
secret headers or raw sensitive exceptions. Error text is safe display text;
schema validation alone cannot establish that it contains no secrets.

Stage an acquisition privately, then atomically publish its immutable directory
with `result.json` written last. A terminal message references that file and its
hash. Cancellation/interruption without a validated terminal/result is incomplete;
existing files alone do not establish success. Retry uses a new attempt ID and
does not mutate a previous immutable result. Safe reuse verifies request identity,
source snapshot/representation, file integrity and coverage, not existence only.

`completed` means every requested window has complete requested/context coverage
after allowed context clamping, dependency-complete artifacts and no errors.
`partial` means at least one independently usable clip exists, but at least one
requested window is incomplete; include errors and per-window states. `failed`
means no acquisition result can be accepted for this attempt; preserve diagnostic
state but do not expose partial files as approved artifacts. Failed-before-resolve
results use null source/representation and empty windows/clips. Failed results
have an empty clips array. Both partial and failed statuses exit nonzero.
Completed exits zero. Status and process exit must agree.

Boundary validation means structural probing plus independent decoding around
both ends sufficient to check declared dependencies. It does not claim complete
full-clip decoding or pixel/alignment acceptance. CVA subsequently validates the
complete decoded frames it actually uses. Downloader validation must not require
network access to make the delivered MP4 readable.
The per-clip `requested_coverage_complete` flag covers the portion assigned to
that continuous clip span. It does not claim one split clip covers an entire
multi-period window. The window result and consumer coverage check establish
the union of its referenced clips.

## Timeline sidecar

Each clip has a UTF-8 JSON Lines sidecar, one schema-valid row per encoded video
sample, in clip decode/sample order. `sample_index` starts at zero and is
contiguous. Each row binds to the containing clip ID. Time bases and period IDs
come from that clip's result entry. No sample may cross an unreported period or
configuration change.

Each row supplies clip/source PTS, DTS and duration ticks, mapped original episode
presentation time, random-access indication and both source/clip sample payload
hashes. PTS need not increase in decode order. The consumer must account for
reordering, edit lists, composition offsets and DASH period/presentation offsets.
`episode_time_seconds` is the sample's source presentation position on the
selected episode's visible timeline, not its DTS or a byte/segment offset.

Roles are `requested_content`, `analysis_context` and `decode_dependency`.
Use requested-content precedence when a sample intersects a requested interval;
otherwise context precedence over dependency. Roles are interval classification,
not donor-cleanliness or mask approval. Boundary-straddling samples remain whole.
The renderer selects the exact needed decoded frames after alignment.

An encoded sample is not necessarily one decoded frame. CVA independently builds
decoded-frame indices, presentation timestamps and durations. Do not infer global
episode frame numbers by multiplying a clip timestamp by nominal frame rate.
Piecewise source-to-episode mappings remain possible across separately delivered
clips. A single guessed timestamp offset must not hide discontinuities.

## Semantic checks beyond JSON Schema

The implementation must additionally check:

- Unique request window IDs and unique clip IDs/artifact paths; valid references.
- Canonical rational values, ordered positive-length intervals, anchor sign rules
  and resolved context containing the resolved requested interval.
- All requested window IDs represented in terminal results; no invented windows.
- Completed window coverage from referenced clips, including required context
  and dependencies; no implicit coverage across a missing period/gap.
- Pinned source/version/representation identity and expected snapshot agreement.
- Exactly one video stream per clip and no donor audio/subtitle streams.
- File sizes, SHA-256 values, real codec configuration and reported properties.
- Complete, contiguous sample tables consistent with actual packets; timestamp
  rescaling and episode mapping without undocumented rounding or drift.
- Source/clip payload identity, or an explicit supported framing transformation
  with evidence; no hidden re-encoding.
- Exit/terminal/result agreement and safe filesystem publication.

## Alignment, masks and renderer boundary

Only CVA establishes visual correspondence. Matching requested times, nominal
frame rates, audio locales or durations do not establish matching animation.
Establish piecewise temporal and spatial mappings and account for crops, scaling,
animation variants and episode edits. Uncertain matches remain unresolved.

Rectangles are useful candidate/search areas. General compositing requires
binary/grayscale masks that can be irregular and time-varying at recipient frame
coordinates. Rectangles remain a supported simple case. Feathering must remain
inside the approved mask. Conflicting overlapping patches require explicit
ordering or rejection. Different patches may refer to different immutable donors.

The recipient is the image/timeline authority. Normalize color interpretation,
compare matching unmasked areas, and approve a stable per-scene donor-to-recipient
color transform. Do not neutralize intentional scene grading or alter the
recipient outside masks. Color correction and spatial resampling are renderer
operations, never acquisition operations.

Retain immutable sources and a versioned edit plan. Bind approvals to source
hashes, temporal/spatial mappings, masks, color transforms and renderer policy.
Apply all accepted edits from original sources together. Previews are disposable
and never become final-render input. A partial restoration needs explicit review
and must identify unresolved sections; partial acquisition cannot approve it.

The decoded composite must equal recipient pixels wherever the mask is zero.
Lossy final encoding can change pixels outside masks; strict pixel identity in a
published master therefore requires an appropriately configured lossless path.
Lossless coding does not undo prior lossy scaling/color conversions. FFV1 in
Matroska is a proposed working intermediate, not the downloader delivery format.

Investigate copying untouched encoded sections and encoding affected dependency-
safe spans. Do not assume independently encoded spans concatenate safely. Codec
parameters, initialization, reference-frame closure, PTS/DTS, duration and join
decoding must be compatible and validated. Until qualified, do not advertise
smart rendering or bit-identical untouched compressed sections.

Preserve recipient audio and embedded subtitle payloads and per-packet timing,
including delay/skip/edit-list effects. Preserve external sidecar bytes and
timeline association. No donor audio is spliced in by this restoration operation.
Verify recipient video presentation timestamps, frame durations/count and extent.
New OCR/transcription artifacts are distinct from existing recipient subtitles.

## Reuse and reference lifetime

Reuse approved reference packages: original useful donor clips, fingerprints,
verified clean coverage, masks, mappings, transforms and approval history. Do not
blindly reuse a lossy restored opening across recipients with different credits,
framing, colors or animation. Recipient-specific compatibility remains required.
Keep references while active jobs or schedules need them; cleanup must respect
shared references and never delete published media under a log-retention policy.

## Integration status and acceptance criteria

Inspected CVA `main` at `2bcd90bbcf3a706fcf355084adf0a71e6ea551c4`, including
the uncommitted local restoration workflow. It reads local MP4s, hashes original
sources, supports different donors per fixed rectangular patch and renders from
the recipient's native clock. The current implementation is not this contract:

- Origin-zero clip clocks need explicit clip-to-original mapping support.
- Comparison currently assumes compatible image properties/equal frame durations.
- General irregular/time-varying masks and per-scene transforms remain future work.
- Analysis windows are capped at 180 seconds. Five-minute acquisition needs
  bounded subwindow inspection, not an unreviewed increase in comparison limits.
- The renderer currently re-encodes the full video with H.264 CRF 18.
- Existing stream hashes/coarse stream timing need stronger per-packet timing
  verification, and existing recipient external sidecars need explicit binding.
- Mandatory reviewed transcription must be decoupled from the acquisition API.

The existing CVA Protocol v1 adapter requires a singular MP4 with video/audio.
Its strict validator does not support this video-only multi-window result.
The new entry points and transport below are now specified, but remain
unimplemented/unaccepted until separately qualified. Existing ordinary download
CLI flags, text mode and `cva-downloader-progress` version 1 behavior remain
unchanged. This contract introduces no new automatic retry or authentication
policy. Continue using the approved private authentication handoff without
adding credentials to these records.

### Capability query and CLI

- `crunchyroll-downloader --capabilities-json` writes exactly one UTF-8 JSON object
  matching `capabilities.schema.json`, followed by a newline, then exits zero.
  It performs no sign-in, browser launch, media/network request or acquisition.
  Advertise `media_windows_v1` only when implemented and qualified, not because
  schemas/scaffolding exist. A tool implementing the query but not this operation
  returns `operations: []`. Unknown/missing query support means unsupported, not
  permission to fall back to a whole-track download.
- `crunchyroll-downloader --request-json PATH` reads one UTF-8 JSON request file,
  maximum 1 MiB, and selects this media-window mode. Pass PATH as one process
  argument, without embedded shell quotes. Resolve it as a native filesystem path.
  Reject conflicting acquisition/output/progress-mode CLI flags. Source, stream
  selection, boundaries and output directory come exclusively from the request.
- The media-window mode emits `cva-media-window-progress` version 1 JSON Lines on
  stdout. This is a different protocol name/version namespace from legacy
  `cva-downloader-progress` version 1. Do not add these events to the legacy parser
  or change existing download progress semantics.
- Exit zero only for a validated completed acquisition. Exit 1 for partial or
  failed acquisition, including invalid requests. Capability query success exits
  zero. Transport status/result/exit must agree; transfer at 100% is not completion.

### Media-window progress events

Every record has `protocol`, `version`, a contiguous integer `sequence` starting
at 1, `request_id` and an invocation-scoped `attempt_id`. Emit one object per line,
newline terminated and flushed, with no ANSI or human prose. Each line is at most
65,536 UTF-8 bytes including its newline. Diagnostics may use bounded stderr but
are not completion authority and must contain no secrets or raw sensitive errors.

- `phase`: initializing, authenticating, resolving, downloading, assembling,
  validating or publishing. Repeated phase reports do not reset transferred work.
- `window_progress`: a known `window_id`, `unit: segments`, integer completed/total
  units and integer percent. Total is positive; completed is between zero and
  total. Percent equals floor(100 * completed / total). Counters must not regress
  within that window/invocation. Freeze total before the first transfer-progress
  event for that window. Omit this event until total is known; report
  phases instead. These counters measure transfer, not validation or publication.
- `warning`: an allowlisted nonfatal code and safe display message.
- `terminal`: exactly one, with completed/partial/failed status, result-file path
  and SHA-256 when available, nullable typed error code, and safe display message.
  No records may follow the terminal. It is also legal for an early failure to
  emit only the terminal with sequence 1.

Completed and partial terminals require a published `result.json` with a matching
hash, request/attempt IDs and status. A completed terminal has null error code;
partial and failed terminals have a typed code matching the reported failure.
The terminal result path is exactly `result.json`, relative to the request's
output directory. Emit it only after atomic publication, never while the file or
its referenced artifacts are still being written.

A failed terminal may have null result when no trustworthy result can be
published, such as an invalid request or unwritable/unauthorized output location.
Do not create an output directory from an unvalidated request merely to publish
an error. If the request ID cannot be safely recovered, use null request_id,
failed status, null result and `invalid_request`; a non-null attempt ID still
identifies the invocation. A normal failed result file, when safely publishable,
continues to obey `result.schema.json`.

The receiver must reject malformed/oversized records, unknown protocols/versions,
sequence gaps, ID changes, missing/duplicate terminal events, post-terminal data,
invalid progress arithmetic and result/hash/exit disagreement. A crashed process
or missing terminal is incomplete, never completed. A terminal record alone does
not waive result-schema, artifact, timing or media validation.

Downloader acceptance should include independently decoding delivered MP4s with
network disabled; exact requested/context/dependency coverage; B-frame/open-GOP
boundaries; nonzero/negative timestamps; short/overlapping/end-relative windows;
VFR; rejected codec-configuration changes and compatible discontinuity splits;
explicit boundary policies; pinned follow-ups; video-only delivery; source and
sample identity; cancellation/retry/partial failure; and unchanged source files.
Renderer acceptance is separate and must prove visual/mask approval, recipient
timing/payload preservation and the chosen lossless/lossy output guarantees.

## References

- [FFmpeg seeking and stream-copy behavior](https://ffmpeg.org/ffmpeg.html#Main-options)
- [FFmpeg concat compatibility and boundary caveats](https://ffmpeg.org/ffmpeg-formats.html#concat)
- [FFV1 lossless intermediate format, RFC 9043](https://www.rfc-editor.org/rfc/rfc9043.html)

The schemas validate data structure. This contract's semantic and media checks
remain mandatory even when a JSON Schema validator reports success.
