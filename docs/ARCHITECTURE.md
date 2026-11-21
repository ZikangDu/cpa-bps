# Architecture

```text
API client -> CPA HTTP API -> BPS provider plugin -> authorized BPS Responses API
                  |
                  +-> caller-scoped Files store
                            |
BPS plugin -> host.documents.read -> local document worker -> text/page images

Manager -> CPA management API -> plugin account/plan/quota/quality endpoints
```

CPA loads the `.so` into its process. The document worker is a short-lived child
process without a listening port. The Manager has its own existing management
listener; it is not an additional inference gateway.

## Request handling

Automatic mode classifies the declared request structure. Plain text, image and
JSON-only requests keep the lightweight streaming path. Tool requests select a
text or native relay based on their capability family. Success is retained only
for the same account/caller/model/conversation/family. A change from a simple
function to a code/custom/multiple-tool request is evaluated again.

A complete tool response is validated before delivery. When possible, another
local decoder interprets the same response without an additional generation.
Known unexecuted protocol failures can switch protocol once, with at most three
generations per selected-account execution. HTTP refusals, uncertain transport,
partial output and unknown hosted actions are not replayed as protocol recovery.
CPA account failover is a separate bound. Missing usage remains unknown.

## Documents

See [document processing](../plugin/DOCUMENTS.md). File ownership uses the
client's authenticated caller scope, not an upstream OAuth account. Files survive
upstream account changes but are never readable by another caller. Excel macros,
formula recalculation and external workbook links are not executed. PDF scripts
and remote links are not executed/fetched. Explicit extraction limits and omitted
coverage are included in model input.

## Persistence

- CPA auth directory: OAuth credentials and host-managed metadata.
- Rebuilt plugin state directory: concurrency overrides and quality schedules.
- Manager data directory: existing encrypted setup and usage SQLite database.
- Files directory: private uploaded documents with expiry.
- Process memory: bounded attachment/parser caches and session transport choices.

A service restart resets in-memory affinities/caches. Plan migrations and staged
validation so routine upgrades do not cause repeated restarts.
