# Optional caller-scoped document Files API

Set `CPA_DOCUMENTS_DIR` to an absolute private directory before starting CPA.
The server creates it with mode 0700 and stores binary and metadata files with
mode 0600. If unset, `/v1/files` routes return 503. No new service or listener is
started. Parsing remains in the BPS plugin.

Supported routes use the ordinary authenticated API caller identity:

- `POST /v1/files`: multipart `file` and `purpose` (`user_data`, `assistants`, or
  `vision`). Accepts PDF, XLSX, XLSM, XLTX, XLTM, legacy XLS, and CSV/TSV text containers (including UTF-16 BOM and GB18030-compatible bytes; the worker validates the encoding).
- `GET /v1/files`: caller-owned metadata list, optionally filtered by `purpose`.
- `GET /v1/files/:id`: caller-owned metadata.
- `GET /v1/files/:id/content`: private attachment download.
- `DELETE /v1/files/:id`: revoke access and remove stored files.

Default limits: 20 MiB per file; 20 files and 256 MiB per caller; 2 GiB per store;
24-hour expiry. Expired entries are removed on startup and store operations.
Four multipart uploads may buffer concurrently. Each successful upload returns
`file-cpa-doc-<random>` plus `expires_at`; upload success means bytes are stored,
not that the plugin has already parsed the document.

The plugin reads a reference using:

```json
{"host_callback_id":"the execution callback ID","file_id":"file-cpa-doc-..."}
```

through `host.documents.read`. The host validates callback lifetime, calling
plugin and instance, then derives the caller namespace from that callback's
original authenticated context. The plugin cannot supply an arbitrary owner.
The result is `{filename,mime_type,data,bytes}`; `data` is JSON base64 from `[]byte`.
Missing, expired or cross-caller IDs are unavailable. Upstream OAuth account
selection or failover does not change file ownership.

No document payloads are recorded by the request/response logger on Files API
routes. Other inference routes keep the operator's existing logging behavior.
Uploaded documents are never evaluated or rendered by the core server.

Validation includes caller isolation across list/read/delete and callbacks,
forged/closed/canceled callback contexts, persistence, expiry, size and storage
quotas, concurrent creation, symlink/path rejection, multipart validation and
Files-route logging exclusion. The local build command is:

```sh
go test -race ./internal/documentstore ./internal/pluginhost ./internal/api ./internal/api/middleware -run 'TestStore|TestDocument' -count=1
go vet ./internal/documentstore ./internal/api ./internal/api/middleware
# pluginhost production files were vetted separately to exclude the baseline test warning.
go build -o /tmp/cpa-bps-documents-056 ./cmd/server
```

Full test-enabled vet currently reports a pre-existing `sync.Once` copy in
`internal/pluginhost/host_test.go:1637`, outside this change.

The native build requires Go 1.26.10 or newer and CGO enabled for dynamic
plugin loading. Linux verification uses an isolated module/build cache.

## Chat Completions document replies

The document upload integration exposed an existing translation mismatch:
plugins may return a standard bare non-stream Responses object, while the
Codex-to-Chat converter previously accepted only a terminal event envelope and
silently returned an empty body. The converter now accepts both standard
completed/incomplete forms and preserves message content, tool calls, usage,
and finish reasons. Nonterminal or malformed objects are still rejected.
Translator and plugin-adapter regression tests cover this document-client path.
