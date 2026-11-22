# 0.5.10-reconstructed: protocol and lifecycle audit

This release follows the 0.5.9 production fix with original-plugin comparisons,
real model requests and isolated concurrency/document tests. The original plugin
remains a retained 0.5.31 binary; this repository is a maintained reconstruction.

## Reproduced tool conversion failure

A synthetic `gpt-6-sol` request declaring `read_file`, `write_to_file` and
`replace_in_file` reproduced HTTP 422. Its complete upstream response contained
an otherwise valid `run_officejs` relay with
`code={"name":"read_file","arguments":{"path":"fixture.txt"}}` and
`references=["fixture.txt"]`. The reconstruction treated the file reference as a
legacy tool selector and rejected it. Feeding that identical response into the
original binary produced the correct `read_file` client call.

Legacy references now select tools only when they name a declared tool. A complete
explicit name/arguments envelope remains authoritative over ordinary file or
workbook references. Conflicting declared tool selectors still fail validation.
Original-plugin captures also confirmed JSON-string-encoded function arguments;
these are decoded locally and validated against the full declared schema.
Neither fix executes a tool or requires another generation.

Cross-model acceptance additionally reproduced Luna returning undeclared built-in
actions through the text relay, for both optional and required client tool calls.
The same synthetic request succeeds through native transport. Automatic mode now
starts all callable client tools on the original native transport, independent of
model name. Requests with no callable tools keep the text path. Required, optional
and no-call contracts have separate learned capability keys. The bounded safe
fallback remains; unknown hosted actions never authorize replay.

Text tool relays accept a complete final-message JSON envelope after commentary,
without searching arbitrary prose for JSON. A preceding JSON example, later
prose, multiple envelopes, refusal and unfinished responses do not become tool
execution. Native repair follows the same complete/unexecuted-response safety
gate as automatic transport. Public errors expose fixed stage/reason categories,
never tool arguments, upstream messages, document text or credentials.

## Lifecycle, SSE, cache and documents

- A small optional host callback capability propagates caller cancellation through
  documents, queue admission, uploads and direct/automatic/native generations.
  Normal completion and plugin unload release observers. C ABI is unchanged;
  older hosts retain their previous cancellation behavior.
- An allowlisted quota error inside HTTP 200 SSE refreshes account admission for
  subsequent requests, even if the client closes after receiving that error.
  The current partial stream is not replayed. Error frames retain safe machine
  diagnoses. Conflicting SSE event/status terminal pairs cannot deliver tools.
- Empty histories no longer panic. Native custom-tool reminders have deterministic
  ordering, preserving identical request prefixes for repeated catalogs.
- Canceled document parsing no longer poisons parallel waiters or the shared cache.
  Explicit TSV preserves tabs despite commas/semicolons in headers. Documents
  recovered from legacy host translation retain their original turn/call position;
  ambiguous recovery fails explicitly.

## Validation and retained failures

Original-binary offline captures and the reconstruction run with synthetic TLS,
credentials and disabled external networking. The final comparison covered
49 original cases and 55 candidate cases, with 213 checks passing, including the
actual failing relay and encoded-argument boundaries. Full plugin race/vet and isolated native
ABI checks cover the new behavior.

Document ABI validation passed 26 cases, including PDF, scanned/mixed PDF,
XLS/XLSX/CSV/TSV, Files caller isolation and Chat streaming. Concurrency tests
covered 64 simultaneous clients for both session identity inputs, 12 simultaneous
429 failovers, full three-account traversal and canceled queue/generation work.
The old cancellation behavior fails the real TCP disconnect test; the fix drains
slots and closes upstream connections without replay.

The production 0.5.9 baseline completed 17 of 18 model/stream combinations; one
`gpt-6.2-sol` SSE returned an upstream rate-limit error. Quota inspection found one
exhausted account and other low balances. Further acceptance uses a separate
loopback instance and an access-token-only copy of a healthy authorized account,
without copying its refresh token or changing production account flags. The XLS
answer initially failed an exact-string assertion: the model returned `40.0`,
which is numerically correct. Initial failures remain in private evidence.

The host full suite retains its known Retry-After assertion mismatch (10 versus
7), and its pre-existing test vet warning. Do not describe the entire upstream
CPA suite as green. Final build/live/deployment results are recorded below once
completed.
