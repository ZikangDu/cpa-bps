# Reproducible Manager frontend

`base.html` is the MIT-licensed CPA Manager Plus single-file frontend distributed
with BPS 0.5.31. Its preserved SHA-256 is
`98f2728ecddb0ca7cff89a6f0f395a876085704cd0101ed370eff4db8b646db3`.
The adjacent license is preserved. The original closed plugin binary is not a
build input.

Current editable frontend code is in `plugin/panel/*.js` and `compact.css`.
`plugin/panel/patch_manager.py` applies these components deterministically to the
pinned base. It rejects an incompatible base rather than silently patching the
wrong component. Reapplying it is idempotent.

```sh
python3 plugin/panel/patch_manager.py --input panel/base.html --output dist/management.html
node --test plugin/panel/*.test.cjs
```

The complete upstream Manager source and our backend plan enrichment are kept
under `manager/`. A plain build of its unmodified React source does not reproduce
the BPS extensions; use the frontend patch build above until those extensions
are migrated into its TypeScript source tree.
