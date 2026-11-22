# 0.5.9-reconstructed: request compatibility

This release fixes two independently reproduced compatibility errors against the
original BPS 0.5.31 plugin and the BPS endpoint.

## HTTP 400 for `reasoning.effort: "max"`

CPA validates model capabilities before calling the plugin. The reconstructed
model directory omitted client aliases, so that validation rejected `max` before
the plugin could normalize it. Original-plugin captures advertise `max` and
`ultra` where `xhigh` is supported, and send `xhigh` upstream.

The plugin now advertises `max`, `ultra` and `x-high` only when the account's
upstream catalog supports `xhigh`, and normalizes them before dispatch. Other
providers and models without that capability retain their existing validation.
There is no production-code change to CPA's generic thinking policy.

## HTTP 422 with images in tool results

BPS rejects an `input_image` nested inside a tool result's `output` array. In a
bounded A/B probe, the same uploaded synthetic PNG returned HTTP 422 in that
location and HTTP 200 when moved to the original plugin's attachment layout.
The failure was independent of the image's file identifier or account quota.

Both native and text tool transports now replace each image's original position
with a reference and append an image message directly after that tool result.
Text, image ordering and call associations remain intact. Text mode sends actual
attachments rather than encoding image data into the textual tool result.
Normalization does not mutate the original history or create a new client turn:
session/cache keys, turn IDs and iteration counts are preserved.

The text transport also uses the original plugin's reasoning envelope rules:
omit history entries without encrypted content and retain valid ciphertext in
its supported envelope. It strips only known client-only item metadata.

## Diagnostics and checks

The known upstream `422: Invalid request body.` wrapper is reported as
`bps_request_validation_failed`. Standard validation arrays expose only bounded,
allowlisted machine codes and field paths. Raw upstream message, input and
context values are never copied into diagnostics. Unknown errors remain unknown;
this change does not add retries or account switching for invalid requests.

Validation includes original-plugin offline captures, model capability tests,
cross-protocol CPA host checks, native/text payload checks and a full local TLS
attachment pipeline with function/custom tools, four images, streaming and
non-streaming replies. The HTTP image regression fails with 422 when the image
normalization is disabled. Existing quota, sticky-session, document and tool
regressions remain part of the plugin suite.

## Production acceptance

Deployed on 2026-11-22 (Asia/Shanghai) from application commit
`75924e1495db49f6d5263bcc58292c63fdb75363`. API image:
`sha256:d6736a4253d7bab834afd21751590ae531ac742a7f81b55c7645f350ecb890e9`.
The unchanged Manager continues on the verified 0.5.8 image. Both services are
healthy. Seven credentials, including four BPS accounts, retain their stable
identities and disabled flags. The existing configuration was unchanged.

Final public-HTTPS checks all passed on `gpt-5.6-sol`:

| Synthetic request | Result | Elapsed |
| --- | --- | --- |
| `max`, plain non-streaming Responses | HTTP 200, exact answer | 2.96 s |
| `max`, 193 history items, 44 custom tool call/result pairs, four tool-result images, SSE | HTTP 200, correct image answer | 5.70 s |
| `ultra`, simple function result with an image, non-streaming Responses | HTTP 200, correct image answer | 3.19 s |

Each response's input/output/cache token counts matched its Manager record by
request trace. These are individual smoke measurements, not a speed guarantee.
The full plugin race suite, plugin vet, targeted CPA host regression and 24
deployment/migration tests passed. CPA host tests use `-vet=off` for the existing
upstream sync.Once-copy warning; plugin vet ran normally.

The prior image and private runtime backup are retained. No reset cards were
consumed. See [OPERATIONS.md](OPERATIONS.md) for build and rollback instructions.
