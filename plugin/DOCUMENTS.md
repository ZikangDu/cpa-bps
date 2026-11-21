# Automatic PDF and Excel analysis

Version 0.5.6 adds local document preprocessing. The client can submit files with
Responses `input_file.file_data`, Chat Completions `file.file_data`, or upload
through `/v1/files` and reference the returned `file_id`. The application no
longer needs to convert files into text or page images itself.

## Processing

- XLSX/XLSM and Excel templates: preserve worksheet names, visibility, cell
  coordinates, displayed/raw values, saved formulas, cached-value availability
  and merged ranges. Formulas are not recalculated, macros do not run and
  external workbook links are not fetched. Charts are reported as a limitation
  instead of silently claiming to interpret workbook drawings.
- Legacy XLS: use saved cell values, dates and merged ranges. Formula source is
  not available through the legacy reader; this is explicitly noted.
- CSV/TSV: accept common Unicode and GB18030 inputs with cell coordinates.
- PDF: extract page text. Scanned pages and pages with graphics, forms or
  annotations also render to page images for the model's vision path.
- Extracted content is quoted document data, never installed as system or
  developer instructions. Page/sheet/cell labels allow answer references.
- A compatible response still depends on the model. Parsing success establishes
  that document content reached the model, not that every answer is correct.

## Upload API and ownership

The plugin ABI cannot register arbitrary public routes. A thin optional CPA
extension in `codex/bps-document-ingress` provides `POST /v1/files`, listing,
metadata/content reads and deletion. It uses the existing API authentication.
`CPA_DOCUMENTS_DIR` enables the private store. No extra listener or gateway is
started. The new `host.documents.read` callback obtains the caller identity from
the original host callback context; a plugin cannot supply another owner.

File ownership follows the client API identity, not a BPS OAuth account. An
account failover therefore retains access to the caller's files. Missing,
expired, deleted or foreign files return the same unavailable response.

Uploads expire after 24 hours. Store directories are private and file data use
0600 permissions. The store enforces 20 MiB per file, 20 files and 256 MiB per
caller, and 2 GiB total. Document upload/download bodies are excluded from CPA
request-body logs. Ordinary analysis requests can still follow the operator's
existing request logging configuration.

## Runtime and installation

`make build` creates the plugin and `build/bps-document-worker`. Put both in the
plugin installation directory. Run `scripts/install-document-runtime.sh` to
create a private Python environment with pinned PDFium and legacy XLS readers.
The PDFium wheel includes its rendering library. There is no persistent parser
service; each uncached document is processed in a bounded child process.

Configure the plugin's `document_worker_path` and `document_python` once during
installation. Defaults are `plugins/bps-document-worker` and
`plugins/document-runtime/bin/python`. Set `CPA_DOCUMENTS_DIR` on the local CPA
launcher to enable standard file uploads. Inline file_data does not require the
new upload routes.

The reconstruction includes the implementation source for both the plugin and
the thin CPA extension. Installing only the plugin on an unmodified older CPA
supports inline document parsing when the helper is installed, but does not add
`/v1/files` to that older host.

## Bounded processing and explicit coverage

Each request accepts at most four documents and 40 MiB of decoded document data;
each file is at most 20 MiB. Excel extraction is bounded to 32 sheets and 20000
populated cells. PDF extraction is bounded to 100 pages, with at most eight
rendered pages and 16 MiB of page images. Extracted document text is limited to
512 KiB across the request. Any omitted content is explicitly marked with file
and page coverage; the prompt instructs the model not to imply full-document
coverage. These are limits, not promises of unlimited file analysis.

Parser concurrency is two jobs. Private temporary files and child process groups
are cleaned up on completion/timeout. A caller-scoped, content-addressed cache
coalesces repeated parsing for five minutes (64 MiB total); no OAuth token is
part of it. Parser failure does not trigger model generation. Password-protected
files must be unlocked; arbitrary remote file URLs are not downloaded.

The primary Excel library is Excelize (BSD-3-Clause); PDFium/pypdfium2 and xlrd
licenses must accompany packaged distributions. `build/licenses` is generated
for the provided build artifacts.

## Release evidence

Mac and Linux each passed 25 document end-to-end cases. Parser and plugin race
tests passed. Live model verification stopped at upstream HTTP 403 on the first
workbook; no further generation was attempted and it is not counted as passed.
The CPA full suite has one unchanged baseline Retry-After assertion failure;
all targeted document and response-compatibility checks passed.

[Release report](../docs/VALIDATION.md).
