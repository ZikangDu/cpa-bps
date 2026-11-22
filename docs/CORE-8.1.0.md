# CPA 8.1.0 with the existing BPS customizations

This core upgrade starts from stable application commit
`7ba417630072c17589cfd209e2b17bdeadc3b880` and merges the official CPA `v8.1.0`
tag, commit `6960763455c4ac3d4828fb1736aa5de2183ccecf`, against the original
`v8.0.75` base `c19787b25262b5e6e46a9e8da62f4333e7e478bc`.

The CPA core reports `8.1.0-bps.1`. The BPS plugin keeps its separately versioned
`0.5.11-reconstructed` release. The build injects a valid UTC build timestamp,
fixing the dashboard's `Invalid Date` caused by the old `unknown` build date.

## Preserved custom behavior

The three-way import preserves the existing 25 modified and 16 added CPA files,
including their regression tests. Two textual conflicts in Antigravity refresh
imports/test setup were resolved by retaining the new upstream refresh behavior
and the existing external OAuth registration configuration. All other overlapping
changes were reviewed rather than replacing the new core with old whole files.

- Caller-isolated `/v1/files` CRUD, the document store, `host.documents.read`,
  document lifecycle cleanup, and exclusion of uploaded file contents from logs.
- `host.request.open` / `host.request.wait`, the optional request-lifecycle
  capability and callback/instance ownership. A disconnected caller cancels BPS
  uploads/generation and releases queue slots; an older-host compatibility mode
  is not inadvertently selected on the new host.
- Responses JSON and SSE-to-Chat translation, alongside upstream refusal and
  terminal text handling. Preserve streamed text without duplicate terminal
  output or missing nonstreaming content.
- Top-level plugin usage accounting even when input details contain cache
  retention metadata, alongside upstream optional usage attribution and TTL
  cache-write counters.
- External Antigravity OAuth client configuration. Tests use synthetic local
  registration values; production does not regain embedded registration secrets.

BPS tool relay repairs, 100-image histories, bounded parallel uploads, account
quota gating, cache partitions, first-text streaming, quality defaults and the
credential UI remain in the unchanged plugin/Manager source. ABI version remains
1 and RPC schema remains 10, with optional upstream capability additions.

## Verification

Use the new core binary for every native-ABI test; fixture-only plugin builds
that redirect BPS hosts to synthetic loopback endpoints must never be deployed.

Initial full-core testing exposed new upstream Antigravity tests assuming an
embedded OAuth registration. Synthetic configuration was added at their existing
test entry points without removing assertions, skipping tests or changing
production authentication behavior. An unused local `sync.Once` declaration and
copy in an upstream lifecycle test was removed so full `go vet` can run without
the previously recorded copy-lock warning. Final suite results, exact artifacts,
real request evidence and production provenance follow after validation.

The final ordinary `go test ./...` run completed 108 test-bearing packages with
27,910 passing test/subtest events and 16 existing conditional skips, zero
failures. Thirty additional packages report no tests. Full `go vet ./...` passed.
Native-ABI tests using the new core passed document 27/27, image 36/36,
64-client affinity/failover, error follow-up, cancellation and TTFT 13/13.
Two upstream test-harness issues were additionally reproduced on the unchanged
v8.1.0 tree during the full race run. The replay-delivery fixture now gives its
logger and executor the same request-logging configuration, with all original
assertions and new error-only scenarios retained. A large-JSON test keeps its
functional and 64 KiB allocation gates under race; its non-instrumented latency
measurement is now a benchmark rather than applying a 20 ms wall-clock limit
to race-instrumented scans. Original and merged benchmark medians match (about
3.3 ms split / 2.4 ms unsplit); no production code or test threshold was relaxed.
The complete corrected race run passed all 108 test-bearing packages (27,907
passing test/subtest events, 20 existing conditional skips), with zero failures.
Full vet passed again on the final test tree.

Required integration coverage includes Files/document parsing; Chat/Responses
conversion; nine-model/effort registration; 100 unique/repeated images and tool
history; 64-client affinity/failover; HTTP/SSE/JSON quota follow-up; request
cancellation; and first-text streaming with late-error non-replay.

## Deployment boundary

Build from a committed source archive. Run an isolated offline stage and separate
bounded real-account checks with no refresh-token copy before cutover. Preserve
all current credentials, concurrency limits, schedules, Manager data and panel.
Only the CPA/BPS application is updated; other Docker projects, ingress and host
services are untouched. Keep the previous image and complete deployment snapshot
for rollback. Record any core-created server identity separately from business
configuration changes.

## Server candidate acceptance

The Linux candidate reused the exact current BPS plugin, document worker and UI
files; their SHA-256 values matched the running deployment. Its core reported
`8.1.0-bps.1` with a valid UTC build date, and its new server identity persisted
in its isolated configuration. Network-disabled rehearsal preserved six
credentials / three BPS accounts and registered all nine models. Linux document
27-case, cancellation and five error-followup ABI suites passed.

Real access-token-only candidate checks passed eight models, a `max` reasoning
alias, function call/result round trip, a gzip Chat request using an uploaded
XLSX file (foreign caller denied with 404), optional native first-text streaming
and 100 distinct images. The streamed prose began at 2.070 s and completed at
3.559 s; the 100-image request completed with the expected visual answer at
43.995 s. These are limited measurements, not performance guarantees.

GPT-6.2 Sol returned the explicit upstream `bps_rate_limited` event on the new
core and on an isolated unchanged 8.0.75 core using the same account and request.
That retained service-side limitation is not marked as a passing generation or
an upgrade regression; no automatic replay was used to conceal it.

## Production acceptance

Deployed on 2026-11-22 (Asia/Shanghai). Core production source is application
commit `d42199790e8dc619409cded8f01edc38b2eafaae`; the subsequent test-harness-only
corrections are in `dae3d7a`. API image:
`sha256:60ba2997057519c393d56029ba59ae4f348e223b75f4bc0d51ac4fc97abf024e`.
Build date is `2026-11-22T06:26:26Z`. The core derives from the exact upstream
v8.1.0 commit listed above; the BPS plugin remains 0.5.11.

The final public HTTPS `gpt-6-sol` / `max` SSE request returned the expected answer
and one successful terminal in 3.612 s; streamed text matched the terminal and
input/output/cache usage matched Manager's stored record. Authenticated version
headers report `8.1.0-bps.1` and a valid build date.

All six credential identities and disabled flags, BPS concurrency limits and
quality schedules are preserved. The only semantic configuration addition is
the new upstream-generated persistent server UUID. BPS plugin, document worker
and UI SHA-256 values match the previous live image exactly. Manager keeps its
existing image. The previous application image and complete deployment backup
are retained for rollback. Other project container IDs, images, start times and
restart counts match the pre-upgrade snapshot.
