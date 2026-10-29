# Authentication progress examples

These JSONL files are captured from the actual CLI using synthetic HTTP and
Python-helper responses. They exercise the production progress emitter and
session provider without using an account, browser, real credential store, or
downloaded media. Each file is a complete version-1 stream. Only `elapsed_ms`
(set to zero) and disposable output paths are normalized; JSON key order is not
significant.

| Example | Scenario | Terminal event / process exit |
| --- | --- | --- |
| [cached-session.jsonl](cached-session.jsonl) | Saved cookie accepted; no browser renewal or storage write | `skipped_existing` / 0 |
| [renewed-session.jsonl](renewed-session.jsonl) | Known-expired cookie renewed, validated, and saved | `skipped_existing` / 0 |
| [unsupported-renewal.jsonl](unsupported-renewal.jsonl) | Missing saved session without a renewal connection, including Docker/Linux launched outside CVA | `failed`, `reason: session_refresh_unsupported` / nonzero |
| [locked-store.jsonl](locked-store.jsonl) | Credential vault locked; no renewal attempted | `failed`, `reason: credential_locked` / nonzero |
| [rejected-cookie.jsonl](rejected-cookie.jsonl) | Explicitly supplied cookie rejected; no automatic account switch | `failed`, `reason: session_rejected` / nonzero |

The first two fixtures intentionally find an existing output after sign-in, so
they end with `skipped_existing`. That event means existence only, not a validated
MP4 or a completed download. For a new output, the invocation continues with the
usual transfer, muxing, cleanup, and terminal events described in the
[protocol documentation](../../README.md#machine-readable-progress-for-cva).

The on-screen text for normal cached sign-in is exactly:

```text
Checking saved sign-in...
Validating sign-in...
Signed in successfully.
```

For a missing or known-expired saved cookie, the sequence is:

```text
Checking saved sign-in...
Renewing sign-in...
Validating sign-in...
Saving sign-in securely...
Signed in successfully.
```

If an apparently valid saved cookie is rejected by the token endpoint, an
additional `Validating sign-in...` precedes `Renewing sign-in...`. A rotated
cookie also produces `Saving sign-in securely...`. A supplied cookie skips
`checking_session`; it is saved only with `--save-cookie`. No authenticated
message follows a failed validation, renewal, or required save. A transient
token-exchange failure does not trigger cookie renewal or storage replacement.

Text mode prints these status lines to stderr. In JSON mode, CVA displays the
same strings from `display_message` on authentication `phase` records. JSON mode
can also emit a plain `phase: authenticating` record without an `auth_step`.
Do not require all steps, assume a fixed event count, or use these steps to
determine download completion. They may repeat during the same download attempt
without resetting track percentages. `auth_step` and failure `reason` are
optional version-1 fields; older emitters remain compatible, and unfamiliar
optional values should fall back to the broad phase or failure code.

## Authentication failure reasons

These are the current allowlisted `reason` values on terminal `failed` events
with `phase: authenticating`. They preserve the broad failure `code`. Additional
safe reasons can be added compatibly. The reason is omitted when no recognized
detail is available; CVA must still handle that failure using `code`.

| Existing `code` | Optional `reason` values |
| --- | --- |
| `credential_storage_failed` | `credentials_missing`, `credential_config`, `credential_unavailable`, `credential_locked`, `credential_corrupt`, `credential_store`, `session_invalid` |
| `authentication_failed` | `session_rejected`, `session_unavailable`, `session_refresh_unsupported`, `bad_credentials`, `interaction_required`, `verification_required`, `runtime`, `browser_runtime_unsupported`, `network_error`, `site_error`, `auth_flow_changed`, `profile_error` |
| `sign_in_configuration_failed` | `dependencies`, `configuration`, `source` |

`bad_credentials` refers to the account credentials used by browser sign-in;
`session_rejected` refers to a session cookie rejected by the token endpoint.
`session_invalid` means the stored or returned session data is malformed.
`session_refresh_unsupported` means renewal was needed but no supported browser
connection is available. Linux/Docker can renew through CVA's active private
authentication connection; without it, this reason remains actionable. It does
not mean the encrypted credential store is unsupported. Container renewal uses
the same `refreshing_session`, `validating_session`, and `saving_session` events
as Windows; there is no new protocol version or required parser field.
A generic token-exchange/network failure may have no reason.

Display the event's safe `display_message`. Do not derive text from raw helper
responses, cookies, tokens, or stderr, or treat any failure reason as an automatic
retry instruction. Only the existing `attempt_failed` / `retry_wait` events
announce scheduled download retries.

## Maintaining the examples

`TestJSONCLIAuthenticationExamples` compares these files with real subprocess
stdout and checks exit codes, sequence, steps, failure reasons, and secret
redaction. To regenerate them after reviewing an intentional contract change:

```powershell
$env:CRDL_UPDATE_PROGRESS_EXAMPLES = '1'
go test -run '^TestJSONCLIAuthenticationExamples$' -count=1 ./...
Remove-Item Env:\CRDL_UPDATE_PROGRESS_EXAMPLES
```

On Linux:

```sh
CRDL_UPDATE_PROGRESS_EXAMPLES=1 go test -run '^TestJSONCLIAuthenticationExamples$' -count=1 ./...
```

Python is required for these offline helper fixtures; FFmpeg is not invoked.
