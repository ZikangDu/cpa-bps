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

Final cache comparison, public-HTTPS checks and deployment provenance are recorded
below after verification. No reset card was consumed.

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
