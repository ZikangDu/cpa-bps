# 0.5.11-reconstructed: comprehensive original-plugin audit

This release includes the [0.5.10 candidate fixes](RELEASE-0.5.10.md) and a second
independent audit against the retained original BPS 0.5.31 binary. The original
Go source is still unavailable; these are verified reconstruction changes.

## User-visible fixes

- Correctly decode explicit tool envelopes when `references` contains a filename,
  and accept the original plugin's JSON-string-encoded function arguments.
- Start callable client tools on the original native transport. Optional,
  required and no-call contracts have separate learned transport bindings.
  Correct a complete, known, unexecuted native relay before switching to text;
  the total budget remains three generations and one transport switch. Initial
  failure diagnostics use fixed categories, without exposing request contents.
- Use `references:["External client tool"]`, satisfying the actual native
  `minItems:1` schema. Legacy empty-array histories remain compatible. Client
  code, commands and custom inputs are not rewritten.
- Preserve a final text tool envelope after commentary, reject conflicting later
  text, and keep all tool calls buffered until their terminal contract validates.
- Reject invalid tool-choice/parallel fields before generation. Reject duplicate
  response item IDs and reuse of historical tool IDs in every transport, without
  replay or cache pollution. Object-valued historical arguments remain intact;
  invalid historical calls fail locally instead of being dropped or rewritten.
- Propagate caller cancellation through parsing, queues, uploads and generation.
  Observed HTTP/SSE/JSON quota rejections survive cancellation. Admission slots
  are released after the quota gate is established, while only the bounded
  metadata observation continues. Subsequent requests can switch accounts;
  failed or partially delivered requests are not silently repeated.
- Resolve model discovery in the selected workspace even when only the ID token
  contains the workspace identity.
- Handle CR, LF and CRLF SSE boundaries, including fragmented/open streams.
  Recognize HTTP 200 JSON errors, sanitize unknown error messages, and reject
  contradictory successful terminal events carrying errors before tool delivery.
- Partition opaque native replay and upstream cache identities by selected
  workspace, including in-place credential replacement. Same-workspace OAuth
  token refresh preserves continuity.
- Fix empty-history panics, nondeterministic custom-tool reminders, canceled
  shared document parses, TSV delimiter handling, and same-byte documents with
  different names/formats. Recovered files stay attached to their original turns.

## Verification scope

The first original-binary round covered 49 original and 55 candidate scenarios,
with 213 checks passing. The second final round used 55 scenarios on each side
and passed 254 differential checks, including source/output fixture equivalence,
negative request cases and exact client-tool reconstruction. Intentional safety
boundaries remain stricter than the original: completed-only delivery, replay
protection, historical-ID protection and refusal to execute ambiguous calls.

Plugin race/vet and targeted host tests passed. The upstream full host suite still
has its pre-existing Retry-After 10-versus-7 assertion mismatch and test vet
warning; it is not reported as fully green.

Isolated native ABI validation covers 64 concurrent clients under both session
identity inputs; account disable/recovery affinity; three-account traversal;
HTTP/SSE/JSON quota failures; cancellation; request-scope/instance isolation; and
parallel unload. Five error-followup scenarios completed 20 client requests with
21 upstream attempts: the initial streamed/JSON failure is retained, subsequent
requests use the healthy account, and no failed generation is replayed.

Document ABI validation passes 27 cases, including PDF/scanned/mixed PDF,
XLS/XLSX/CSV/TSV, Files caller isolation, Chat streaming and recovery of same-byte
files with different names/formats. Three bounded fuzz runs completed 393,820
executions. A 5000-delta, 20,760,000-byte stream passed both forwarding and
aggregation checks without accumulating the whole stream before processing.

## Real requests and retained failures

Real validation uses synthetic prompts and access-token-only copies in isolated
loopback instances. Production flags and refresh-token ownership are preserved.
The final native-reference candidate passed all nine models' function calls,
Luna optional calls, Code/Cmd/Custom calls, multi-turn recall, and the reported
three-file-function call/result round trip. Twenty simultaneous native tool
requests completed in 5.97 seconds with 20 distinct call IDs. Earlier text waves
of 2/5/10/20 clients also completed. These are scoped observations, not throughput
or latency guarantees.

Failures are retained: the production baseline's reproduced 422; undeclared
built-in actions on the old text transport; two 120-second Sol tool deadlines
before correcting the contradictory native reference instruction; and upstream
rate limits during closely spaced GPT-6.2 Sol cache tests. A subsequent bounded
trace confirmed the slow interval occurred in upstream function-argument output,
not local queue cleanup. No incomplete tool call was delivered or automatically
repeated. An XLS answer of `40.0` initially failed an exact-string assertion but
was numerically correct. An initial memory acknowledgment mismatch is retained;
subsequent tests independently check exact recall rather than acknowledgment text.

The observations and production verification below retain failed attempts. No
reset card was consumed.

## Time to first text

Unstructured answers with tool history and no callable tools now stream ordinary
text as it arrives. Optional native-tool requests can stream an explicit
`final_answer` prefix. Required/specific tools, structured JSON and text-relay
tool envelopes remain buffered for validation. After actual text is delivered,
no protocol fallback or generation replay is allowed. Item/part identity and
terminal text must agree; a validated missing suffix is delivered exactly once.
Prefix and text bookkeeping are bounded, with linear text accumulation.

The controlled native-ABI comparison uses the same synthetic stream with a
400 ms gap between the first text and terminal event. Across 13 groups / 51
client requests, the history/no-call case improved from about 417 ms to 7 ms,
and optional native final text from about 417 ms to 8 ms. All 20 concurrent
streams delivered early text; all 20 canceled streams closed upstream and
drained admission. Late errors and invalid tool relays never became successful
completions or triggered another generation after text delivery. These figures
isolate removed proxy buffering, not BPS model latency.

A same-account, same-model (`gpt-6-sol`) real A/B test completed eight requests
(two samples per path and version). First nonempty text-delta medians were:

| Request path | Before | After |
| --- | ---: | ---: |
| Tool history, `tool_choice:none` | 4.893 s | 2.340 s |
| Optional native tools, plain final answer | 5.542 s | 2.717 s |

Every response completed with consistent streamed and terminal text. The new
version delivered text while generation continued, rather than merely sending
headers earlier. These small samples show the observed improvement for this
fixture, not a guaranteed latency or a statistical service-level claim.

## Final acceptance and deployment

Deployed on 2026-11-22 (Asia/Shanghai) from application source commit
`9ca9cfd5076624bb1e9ec026b509a167857ac03c`. The unchanged CPA component was built
with revision `67ccdf31689d1b12c6605cef77611a20360be107`. API image:
`sha256:be90e5296dbeab7ea3d6fd55a9264dec9179ee9d2b3899bc2d9736bb8d87047b`.
The unchanged Manager uses the verified 0.5.8 image. Both services are healthy.
All eight credentials, including the newly authorized fifth BPS account, retain
their identities, disabled flags and concurrency limits. Configuration comparison
found no changes. The pre-cutover snapshot and previous images remain available.

The new account completed all nine models in JSON and SSE (18/18), eight image/
document scenarios, and uploaded XLSX/scanned-PDF Files requests including delete
and read-after-delete checks. During the expanded tool matrix, an intermittent
Luna failure exposed the native-repair ordering issue. After that final patch,
Luna completed ten consecutive optional/required calls, and 20 concurrent native
tool requests all completed with 20 distinct IDs (11.86 seconds for the wave).
Text waves of 2/5/10/20 clients passed 37/37. The reported three-function call and
its result round trip passed. Final Linux image checks include safe follow-up
account switching after HTTP/SSE/JSON errors; the document suite passed 27/27.

Public HTTPS acceptance passed `max` plain Responses, `max` with 193 history
items / 44 custom call-result pairs / four tool-result images over SSE, and an
`ultra` function-result image over JSON. All three answers were correct and their
input/output/cache counters matched Manager records by trace. The exact reported
three-file-function combination and its result also passed over public HTTPS. A
public optional-tool prose response delivered its first real text at 2.578 seconds,
continued through 216 text deltas, and completed at 4.256 seconds; this confirms
that the public ingress also preserves incremental delivery.

The same-account original/reconstructed GPT-6 Sol cache comparison completed six
requests. Excluding each cold request, weighted cached/input ratios were 99.54%
(original) and 99.98% (reconstruction). GPT-6.2 Sol cold requests completed on the
new account, and one original warm sample reported 11,345/24,671 cached input
tokens. Both versions also returned explicit upstream rate-limit errors during
that workload, including after slower spacing; a complete matched warm comparison
was not obtained. These failures are retained and are not a plugin-cache success
claim or evidence of zero-error production reliability.

## Same-version credential UI refresh

The credential table now groups its original action controls below each identity,
using six aligned columns in both single- and mixed-provider lists. The plan
column is 112 px and wraps at underscores into at most two lines; availability
is 104 px. Card mode retains its existing structure. BPS quota fills use the same
`--success-color` token as successful recent requests, with explicit Chromium and
Firefox progress styling instead of browser-dependent accent rendering.

The manual quality-check picker defaults to `gpt-6-astra` when the selected
account's current model directory offers it. Explicit manual selections remain
authoritative, and manual defaults do not overwrite periodic model selections.
Frontend and backend interval defaults are 30 minutes; schedules remain disabled
unless explicitly enabled. Existing deployment schedules are updated to the
requested 30-minute interval while preserving their enabled state and other fields.

Verification used the actual patched Manager bundle with synthetic API responses
at 1512, 1024 and 390 px. It confirmed six aligned table cells, actions below the
identity, a complete two-line plan, no horizontal overflow, matching green
`rgb(34, 197, 94)`, Astra manual selection, a 30-minute disabled schedule dialog,
unchanged card mode and no unexpected mutations or browser errors. Existing
panel suites, quality regression/race checks, full-bundle JavaScript syntax and
patch idempotence passed. The application version remains 0.5.11 as requested.

The same-version UI refresh was deployed from `7237ab8d0c408cb635b176192d59a685b3de751f`.
Its API image is `sha256:fb2d4ac48e341c870df1b0be91daa2f6535042d75ceba22e1ce0608a36c9ff39`;
Manager image is `sha256:63686613b54a23f849874d67af8168b5d9aeebb60421a949179cde0d44fc0e54`.
Both services are healthy. The current six credentials / three BPS accounts were
preserved. All three existing BPS periods read back as 30 minutes, with schedules
still disabled and other configuration fields unchanged. The public application
script and styles match the verified bundle; both container copies have SHA-256
`e7453e1a1005176a5609a2f70eadcc63ea0a9e367e5e6e61352328fff39fed54`.
The prior deployment and quality configuration are retained for rollback.

## Same-version inline image history correction

The reported `bps_inline_image_limit` at 2026-11-22 12:48 (Asia/Shanghai) came
from a `gpt-6-astra` request with 94 history items and 23 different PNG images
inside six custom-tool results. The images totaled 5,190,392 decoded bytes; the
largest was 327,441 bytes. It hit the reconstruction's eight-inline-image gate,
not an upstream size rejection. No generation was sent for that failed request.

Original 0.5.31 offline ABI captures confirmed 100 different images, 1024
repeated image references with one upload, and 24 MiB of total image bytes. Its
per-image boundary is exactly 20 MiB, not the reconstruction's old 5 MiB.

BPS now permits 1024 inline references and 20 MiB per image, with separate local
guards of 64 MiB unique decoded content and 96 MiB encoded data URIs including
repeats. These aggregate guards deliberately bound proxy resource use; they are
not claimed as upstream maxima. The generic Files path retains its previous
8/5/16 limits. Preflight streams strict Base64 through a small hashing/sniffing
buffer and retains encoded descriptors rather than every decoded image. Cache misses use at most four BPS upload workers per request, with a global
eight-upload ceiling and 64 MiB multipart allocation budget. Generic uploads
remain serial. A full pending cache waits cancelably; a failed upload cancels
and joins the batch before any rewritten model payload is returned. Same-request deduplication
also survives global cache eviction. All history positions and call associations
remain; no image is dropped or resized. Credential/origin/protocol partitioning,
TTL, cancellation and bounded singleflight behavior remain in force.

Errors retain the `bps_inline_image_limit` code but identify the budget and its
numeric boundary without exposing image contents. Document rendering keeps its
own eight-page-image / 16 MiB request budget; existing screenshots and file
references no longer silently consume it.

The old/new native-ABI comparison covers auto/native/text modes: the old build
rejects both 23 and 100 images before upload, while the new build passes 27
scenario groups (81 successful synthetic generations). Cases include 100 unique
images cold/hot, 100 duplicates with one upload, a malformed last image with zero
uploads/generations, 20 concurrent requests, cancellation and account isolation.
Boundary tests cover 20 MiB, 64 MiB, 96 MiB and 1024 references, plus padding/CRLF
and equivalent encodings. Strict Base64 property fuzz completed 305,564 executions.
Fixture-only test libraries are never deployment artifacts. Live acceptance and
final deployment provenance are recorded separately after completion.

The first serial-upload candidate completed real 23-image and 100-image requests
with correct visual answers, but the 100-image request took 105.5 seconds even
with some attachments already cached. Bounded parallel upload was added to avoid
turning the corrected image-count support into excessive cold-upload latency.
The original serial timings remain in the private acceptance evidence.

The final bounded-parallel image build passed 36 native-ABI scenario groups
across auto/native/text (144 synthetic generations, including the previous
functional cases). A 100-upload fixture with a fixed 20 ms delay completed in
0.77–0.81 s instead of the 2 s serial delay floor, with peak concurrency four.
Twenty clients uploading four different images each all succeeded, with global
peak concurrency eight. A failing upload canceled its three active siblings,
did not dispatch the remaining sixteen images, and produced no generation.

Deployed the image correction on 2026-11-22 from source commit
`0a4fe43fdfc9bd4fa8d4cd539da8f73dc372b0a4`; API image
`sha256:22b7fb703c5e3440eba460c2193f8294f7b363d6e0f8425b38cd4b267a601db2`.
The Manager image, credential identities, configuration and same-version UI
refresh remain unchanged. Plugin full race/vet and the final Linux document
suite (27/27) passed.

Actual `gpt-6-astra` acceptance used synthetic images with visible random codes.
The first 23 images exactly matched the reported request's per-image byte sizes;
100 unique images totaled 5,436,784 decoded bytes. The 94-item tool history,
100 unique images, repeated 100-image request and 100 duplicates all completed
with the correct first/last visual codes. Cold 100-image processing completed in
46.9 s in the isolated candidate; the hot repeat completed in 12.7 s.

Final public-HTTPS requests also passed: 100 unique images from a cold process
in 44.8 s, its cached repeat in 16.0 s, and the 23-image / 94-item history in 8.1 s.
Each returned HTTP 200, the requested model, one successful SSE terminal and
consistent text. These timings describe this small-image fixture, not arbitrary
image resolutions or a latency guarantee. No user image was resized or silently
removed. Upstream authorization, quota and context limits still apply.
