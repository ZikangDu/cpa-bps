# Component notices

- CLIProxyAPI source (`cpa/`): upstream MIT license at `cpa/LICENSE`.
- CPA Manager Plus (`manager/`, `panel/base.html`): upstream MIT licenses.
- Basispoints protocol adaptation: JaxsonWang MIT license at
  `plugin/internal/bpsprotocol/LICENSE`.
- Excelize v2.11.1: BSD-3-Clause; version pinned in `plugin/go.mod`.
- pypdfium2 v5.12.1/PDFium: component licenses are installed in the Python wheel's
  distribution metadata. The Docker runtime installs the official pinned wheel.
- xlrd v2.0.2: BSD-style license in its installed distribution metadata.
- Other Go dependencies retain licenses in their original modules; exact versions
  are recorded in each module's go.mod and go.sum.

No original BPS 0.5.31 plugin binary is copied into the new runtime image.
