# Validation and known boundaries

The reconstruction has accumulated these validations during local development:

- Original 0.5.31 native ABI comparison: 57 scenarios / 122 checks.
- Automatic transport native ABI: 28 scenarios / 73 checks.
- Real authorized-account automatic transport: 8 groups / 11 client requests.
- Document Files-to-parser integration: 25 cases on macOS and 25 on Linux,
  including scanned/mixed PDFs, XLS/XLSX/CSV, caller isolation and Chat streaming.
- Account quota failover: two- and three-account synthetic native tests;
  exhausted-account concurrency sent no additional upstream generation.
- Session affinity, OAuth PKCE/refresh, tool/result history, images, SSE and
  incomplete-response tool suppression have regression coverage.
- Current release adds stable legacy auth-ID and metadata preservation, with
  synthetic forward/rollback migration tests.

These counts are scoped test results, not an uptime guarantee. Some development
requests failed and are not erased from the private evidence history. In
particular, the document release's real model test stopped at an upstream 403;
its isolated parsing/transport checks passed. The later successful production
smoke results are in [RELEASE-0.5.8.md](RELEASE-0.5.8.md). The follow-up
reasoning-alias and tool-result image checks are in
[RELEASE-0.5.9.md](RELEASE-0.5.9.md).

The upstream CPA full suite has an existing Retry-After assertion expecting 7
where the host's minimum cooldown returns 10; the same failure was reproduced on
the unchanged upstream base. The pluginhost test-enabled vet also reports an
unchanged sync.Once-copy warning. New document and compatibility targeted tests
pass. Do not claim that the entire upstream test suite is green.

Run relevant checks before deployment:

```sh
(cd plugin && go test -race ./... && go vet ./...)
node --test plugin/panel/*.test.cjs
python3 -m unittest discover -s deploy -p 'test_*.py'
(cd manager/apps/manager-server && go test -race ./internal/service/proxy)
(cd cpa && go test -race ./internal/documentstore ./internal/pluginhost \
  ./internal/api ./internal/api/middleware \
  ./internal/translator/codex/openai/chat-completions \
  -run 'TestStore|TestDocument' -count=1)
```

The document worker does not recalculate formulas or execute macros. Legacy XLS
returns saved values, not formula source. Large documents have explicit coverage
limits; unsupported remote URLs are not fetched. The 0.5.10 host/plugin lifecycle bridge now propagates client cancellation to
queue admission and direct upstream operations; old hosts retain their previous
lifecycle behavior. See [0.5.10 audit](RELEASE-0.5.10.md).

## Comprehensive 0.5.11 audit

See [RELEASE-0.5.11.md](RELEASE-0.5.11.md) for the original-binary differential
checks, bounded real-model validation, retained failures and production status.
