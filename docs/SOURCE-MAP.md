# Maintained source map

- `plugin/`: independently reconstructed BPS provider, OAuth, native/text/automatic
  protocol routing, account quota gates, document worker and readable panel code.
- `cpa/`: CLIProxyAPI v8.0.75 at upstream commit
  `c19787b25262b5e6e46a9e8da62f4333e7e478bc`, with caller-scoped Files API and
  Responses-to-Chat reply/framing fixes, correct Responses cache-retention usage
  parsing, and externally configured Google Antigravity OAuth registration.
  See `cpa/DOCUMENT-FILES.md` and `docs/OPERATIONS.md`.
- `manager/`: CPA Manager Plus v1.14.10 at upstream commit
  `a67ed8834a914ab24c36d2490fa6bb6d4166724c`, with BPS plan enrichment in the
  manager-server proxy service.
- `panel/base.html`: pinned MIT frontend base; current components are patched
  from `plugin/panel/`. See `panel/README.md`.
- `deploy/`: reproducible images, sanitized examples and reversible credential
  migration. Runtime secrets and data are deliberately excluded from Git.

This repository contains the maintainable reconstruction, not the issuer's
original Go source. The legacy binary ZIP already present in repository history
is retained only as the original release artifact; it is never a build input for
the reconstruction. Dependencies and copied upstream code retain their own
licenses; see their LICENSE files and `docs/THIRD-PARTY.md`.

Research captures, real request logs, local credentials and installation backups
remain private outside the tracked source tree. Future changes should be made in
the maintained directories above, not in the excluded research copies.
