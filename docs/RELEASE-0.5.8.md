# 0.5.8-reconstructed release validation

Production cutover completed on 2026-11-22 (Asia/Shanghai), using source-built
Linux images. The previous application images, configuration, credential
snapshot and Manager data snapshot remain in the private deployment backup.
No credentials, keys, uploaded documents or production database are in Git.

## Reproducible application source

- API application revision: `b90731ca5026bb3b6817acd095aa76ebecc32ca9`.
- Manager revision: `b90731ca5026bb3b6817acd095aa76ebecc32ca9`.
- CPA: `8.0.75-documents.3`; plugin: `0.5.8-reconstructed`.
- Manager: `v1.14.10-bps.1`.
- API image: `sha256:89b7bbe1b6a4f46e65258fe1cd2c9a0915c8f42613a5ab32f8f0ef0e1ba90de7`.
- Manager image: `sha256:81acbc21d6cca55db91d7f70a8014c1167f9ed572f084d41a5c0b4b799393922`.
- Patched panel: `sha256:bdebe3e20abd10988e955b7c5384d712b06c992b1d824cc91c34376bd3d22992`.

Later commits improve deployment rehearsal/readiness handling and documentation;
they do not change the application sources in these two images. The final
deployment harness was validated with 24 migration/deployment tests. Build and
rollback instructions are in [OPERATIONS.md](OPERATIONS.md).

## Acceptance evidence

- Existing credential IDs, auth indexes, disabled flags and per-account
  concurrency settings survived forward migration. Email and plan metadata
  are available through the deployed Manager API.
- An isolated Manager rehearsal used a consistent SQLite backup and the
  matching data key. Production usage collection is running without errors;
  the cache-accounting database migration completed.
- API rehearsal ran on an internal Docker network without upstream access.
  Its model registration used the existing model catalog; real upstream model
  discovery was separately verified after cutover.
- Both production containers became healthy. Public HTTPS panel, model list
  and Files API returned HTTP 200; requests without a key returned HTTP 401.
  The origin panel matches the pinned output hash. The edge response includes
  an additional script, so its whole-document hash is not identical.
- Eleven document-worker checks passed in the final API image: XLSX, XLSM,
  XLTX, XLS, CSV, TSV, text PDF, scanned PDF, mixed chart PDF, encrypted PDF
  rejection and corrupt PDF rejection.
- Real `gpt-6.1-sol` Responses SSE returned the exact expected short answer
  in 3.27 seconds. Chat Completions correctly analyzed uploaded Excel and PDF
  together in 2.83 seconds. Responses correctly analyzed an uploaded scanned
  PDF in 6.12 seconds. These are individual smoke measurements, not a latency
  benchmark or service-level guarantee.
- The first scanned-PDF checker mishandled the standard `error: null` field;
  its corrected recheck passed. Every uploaded synthetic fixture was deleted
  and subsequent reads returned HTTP 404. No reset cards were consumed.
- Acceptance also found a CPA usage-parser bug: cache-retention metadata could
  hide the real top-level token usage in non-streaming Responses. The release
  fixes this for both complete response objects and bare completed SSE events.
  Sixteen regression combinations and the full parser package race tests pass.
- On the final public-source build, real non-streaming Responses, Chat Completions and SSE
  requests all returned HTTP 200 and the expected answer. Each response's input,
  output and cached-token counts matched its Manager record by request trace.
  The two warm requests reported 17,620 cached input tokens out of 17,623;
  this describes those individual requests, not a general cache-hit guarantee.
- Google Antigravity OAuth registration is now external environment
  configuration, documented in the operations guide. Missing/configured
  registration and refresh paths have targeted tests and race checks. Existing
  valid access tokens remain usable; BPS and Codex do not use these variables.

Local regression coverage and remaining upstream test limitations are documented
in [VALIDATION.md](VALIDATION.md). Production was not load-tested to saturation.
Private machine-readable records are retained beside the server release and
backup manifest; they are intentionally excluded from this repository.
