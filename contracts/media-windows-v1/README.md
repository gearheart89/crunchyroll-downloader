# Downloader handoff — media windows v1

Start with [CONTRACT.md](CONTRACT.md). This bundle contains the accepted design
proposal, five JSON Schema draft 2020-12 schemas, and ten synthetic example
files. It contains no media, credentials, signed locators, runtime captures or
third-party implementation code.

The downloader owns acquisition and exact source/timestamp description. CVA owns
alignment, masks, review and compositing. Source video is stream-copied into
independently readable, video-only MP4 windows with explicit decode padding and
sample-to-original mapping. No video/audio/subtitle compositing is requested.

Handoff revision 2 supersedes the initial bundle. V1 rejects codec-configuration
changes, request windows declare an explicit boundary policy, and the narrow
example pins the resolved representation/snapshot. The capability query,
request-file CLI and separate progress namespace are now specified.

This is not an implemented feature announcement. Acquisition correctness,
capability negotiation and progress transport integration still require
implementation and qualification on both sides. Synthetic
examples are not proof of media acceptance; their repeated-digit hashes and
illustrative timestamps must be replaced with observed evidence.

`SHA256SUMS` lists SHA-256 digests of every other file in this bundle. The ZIP is
a maintainer handoff, not a CVA user release or application build.
