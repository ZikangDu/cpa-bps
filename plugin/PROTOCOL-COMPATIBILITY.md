# BPS 0.5.31 protocol comparison

This reconstruction is source-maintained code, not recovered issuer source. The
reference is the locally held original 0.5.31 binary, executed with synthetic
credentials and loopback TLS in a network-disabled container. No production
configuration, executable, credential, or workbook was modified.

## Implemented in 0.5.4

| Contract | Reconstruction behavior | Evidence |
| --- | --- | --- |
| Native Excel transport | Explicit `bps_tool_transport: native`; `run_officejs` carries declared client tools. No OfficeJS executor is installed. | Original ABI captures and reconstruction ABI cases |
| Excel catalog selection | Native mode uses the original three static protocol selectors (ChatGPT auth mode, Excel agent profile, Basispoints Excel product), plus the selected OAuth account. Authorization and refusals are preserved. | `header-summary.json`; credential-scoped transport tests |
| Function payloads | JSON name/arguments, raw `FUNCTION_CODE`, and `exec_command` `FUNCTION_CMD`, including remaining parameters in `extended_summary`. | Binary round trips, escaping/schema tests |
| Custom tools | Raw input retained, namespace qualified, terminal status completed. | Binary and unit round trips |
| Dynamic catalogs | Top-level and historical `additional_tools` catalogs agree with final validation; nested namespace parents are retained. | Dynamic catalog and namespace tests |
| Tool policies | Declared names, kind, complete JSON Schema, tool_choice and parallel-call restrictions are checked before exposing calls. | Negative ABI and stream cases |
| Native format correction | At most two corrections (three generations total) for a complete unexecuted native relay. Appends explicit `executed:false`; buffers client output. HTTP/transport failures and previously emitted streams are not format corrections. Duplicate IDs are rejected. | Isolated repair ABI and adapter tests |
| Tool history | Same-scope native replay retained; cross-account reconstruction does not reuse another account's native ciphertext. | Protocol round-trip tests |
| Agent history | Visible collaboration context retained; incompatible agent ciphertext replaced by an explicit omission notice, matching the original. Reasoning and compaction ciphertext remain opaque. | Binary captures and leakage tests |
| Context policy | Native mode defaults to 200000, preserves explicit empty arrays and custom policies. Text mode retains the previous default. `bps_compact_threshold` optionally overrides an omitted/null policy. | Binary context cases and configuration tests |
| Images | BPS multipart `purpose=vision` plus file; native references use only type/file_id. Full preflight rejects conflicting references before upload. | Binary multipart capture, upload and cache tests |
| Extra image inputs | Anthropic base64/URL and MCP images normalize to the same image path. This is an extension, not an original-binary capability. | Content normalization tests |
| Document inputs | BPS `input_file` documents are explicitly rejected before generation. Generic configured Responses services retain their own behavior. | Original XLSX/PDF rejection captures |
| Full history | `previous_response_id` without full inline history is not supported, matching the original. | Original and reconstruction ABI cases |

## Deliberate differences and limits

- `auto` is the default from 0.5.5: request capability selects the initial
  protocol, with same-family success reuse and bounded compatibility fallback.
  Native relay support does not imply server-side workbook access.
- Native format correction is optional (`native_tool_repair_attempts: 0..2`).
  With zero corrections, invalid tool responses fail immediately; the original
  may send correction requests. Duplicate identifiers, malformed opaque content,
  undeclared tools and parameter violations remain rejected.
- Native mode with correction buffers the whole response before delivery; it
  preserves the Responses event contract but may delay first visible output.
- No fabricated browser fingerprint, account identity, or tool execution result
  is used. A client must execute a validated declared tool and return its result.
- Standalone remote compaction and server-held continuation are not claimed.
  Inline upstream compaction and opaque compaction history are separate features.
- A live service can change its tool catalog or access policy. Offline ABI parity
  and a successful live fixture do not establish every workbook operation or
  indefinite production stability.

The original captures, reconstruction comparisons, repair comparisons and
attachment comparisons are in
[offline-protocol-054](../docs/VALIDATION.md). Live results and the release
private validation records retain failures as well as successful reruns. The
maintained summary is in [validation](../docs/VALIDATION.md).

## Release validation

The final Linux artifact passed 57 offline ABI scenarios and 122 checks. The
final macOS artifact passed a real-account native-mode CPA suite: 8 groups and
11 client requests on gpt-6.1-sol (text, SSE, history, function/result, custom,
JSON Schema, image, and two concurrent clients). Initial failed probes remain
in the evidence; the final suite passed.

[0.5.4 release report](../docs/VALIDATION.md).

## Automatic transport in 0.5.5

Ordinary text/image/structured requests without client tools retain the normal
streaming path. Tool capability families select the initial tool protocol. The
successful mode is scoped to account, caller, model, conversation and family.
A new request family is reevaluated.

Tool responses are validated before delivery. Alternate local decoding uses the
same complete response when possible. Network fallback switches protocol at most
once; generations within one selected-account execution share a three-attempt bound
and one deadline. CPA account failover is a separate bound. Only known
unexecuted protocol failures qualify. HTTP errors, ambiguous transport outcomes,
partial/failed responses, refusals, unknown hosted tools and old call IDs do not.
Observed token totals include complete discarded attempts; incomplete totals are
omitted and marked unknown with reserved `cpa_bps_usage_complete` metadata.

Buffered tool generations are bounded, but the current direct transport does not
immediately cancel every upstream operation when a client disconnects.

[0.5.5 release report](../docs/VALIDATION.md).

## Documents in 0.5.6

The earlier raw-input_file rejection describes the original 0.5.31 transport
and the 0.5.4 reconstruction. Version 0.5.6 adds a local preprocessing layer
that extracts supported Excel/PDF files before the BPS transport runs. Standard
file_id uploads additionally use the accompanying CPA Files API extension.
See [document processing](DOCUMENTS.md) for supported formats and limits.
