# CPA BPS Reconstruction

A maintainable BPS provider plugin, a compatible CPA host, and the matching
Manager interface. Current release: **0.5.10-reconstructed**.

This is an independently maintained reconstruction. It is not the issuer's
original BPS 0.5.31 Go source. The legacy ZIP already present in this repository
is a historical release artifact and is not used to build the reconstruction.

## What is included

- Independent BPS OAuth login and refresh, account email/plan display.
- Request-aware automatic text/native tool routing, bounded format correction,
  stable sessions and account fallback.
- Account-wide quota waiting: exhausted credentials are skipped until recovery;
  when none is available, requests receive HTTP 429 and a meaningful wait time.
- Automatic Excel/PDF extraction, scanned-page rendering, and authenticated
  caller-scoped `/v1/files` upload/read/delete support.
- Compact Manager account controls, multi-model scheduled quality probes,
  quota/reset-card display, and authorization status below the login workflow.

[Source map](docs/SOURCE-MAP.md) · [Architecture](docs/ARCHITECTURE.md) ·
[Operations and rollback](docs/OPERATIONS.md) · [Validation](docs/VALIDATION.md) ·
[Release acceptance](docs/RELEASE-0.5.10.md) · [Third-party notices](docs/THIRD-PARTY.md)

## Build

Use Go 1.26.10+ and a C toolchain. For a complete Linux image from source:

```sh
docker build -f deploy/Dockerfile --target api \
  --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)" -t cpa-bps-api:0.5.10 .
docker build -f deploy/Dockerfile --target manager \
  --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)" -t cpa-bps-manager:0.5.10 .
```

For local plugin development:

```sh
cd plugin
go test -race ./...
make build
```

The document parser is a bounded local helper process, not a separate gateway.
Its PDF/XLS dependencies are installed in the runtime Docker image. For a native
installation use `plugin/scripts/install-document-runtime.sh`.

The frontend build uses the pinned MIT base and the readable components under
`plugin/panel/`; see [panel/README.md](panel/README.md).

## Deployment boundaries

Use a separate secret/config/data directory. Never commit credentials, private
keys, database files, uploaded documents or request logs. The sample Compose file
is a layout example, not a replacement for an existing production configuration.
An existing 0.5.31 deployment needs the reversible credential migration described
in the operations guide; replacing only the plugin binary is insufficient.

New accounts do not automatically enable scheduled quality probes. The operator
controls their models and interval. Reset cards are consumed only after explicit
UI confirmation and a fresh applicability check.
