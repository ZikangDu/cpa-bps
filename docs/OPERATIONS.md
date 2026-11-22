# Build, migrate, deploy and roll back

Keep a private deployment root containing `config.yaml`, `compose.yaml`, `auths`,
`manager-data`, `secrets`, `logs`, `rebuild-state`, and `document-files`. Never place
that runtime root inside the Git source tree. Existing secrets are retained.

## Build from an exact source revision

```sh
docker build -f deploy/Dockerfile --target api \
  --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)" \
  -t local/cpa-bps-rebuilt-api:0.5.11 .
docker build -f deploy/Dockerfile --target manager \
  --build-arg SOURCE_COMMIT="$(git rev-parse HEAD)" \
  -t local/cpa-bps-rebuilt-manager:0.5.11 .
```

The runtime uses Debian/glibc for native plugin loading. The API image includes
its own parser executable, pinned PDF/XLS runtime and CJK font fallback.

## Optional Google Antigravity registration

Google OAuth registration parameters are external configuration in this source
distribution. For Antigravity authorization or token refresh, pass
`CPA_ANTIGRAVITY_OAUTH_CLIENT_ID` and `CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET` to the
API container through private environment configuration. Obtain these values
from the registration you are authorized to use and keep them outside Git.
Missing configuration produces an explicit Antigravity configuration error.
BPS and Codex do not use these two variables.

## Existing 0.5.31 installation

The old provider stores token fields at the top level. The reconstructed provider
uses `oauth_grant`. Preserve file names, in-container auth directory and original
host auth IDs. `deploy/migrate_credentials.py` stages a separate copy only:

```sh
python3 deploy/migrate_credentials.py /private/old-auths
python3 deploy/migrate_credentials.py /private/old-auths \
  --identity-map /private/identity-map.json \
  --apply --output-dir /private/converted-auths
```

The identity map comes from the existing CPA credential directory and has the
shape `{ "credential.json": { "auth_id": "stable-host-id", "label": "BPS OAuth",
"concurrency_limit": 0 } }`. Keep it private. The migration preserves disabled,
priority, email and other metadata. It never contacts OAuth or extends expiry.
A malformed credential stops the entire batch before writing.

`deploy/release.py` automates the existing two-container layout, preserving
credentials, original auth IDs, Manager SQLite/data key, server keys and account
concurrency limits. It requires Python 3 with PyYAML and Docker Compose on the
host (`python3 -m pip install -r deploy/requirements.txt` if needed). Staging publishes
no ports; the verifier connects to the candidate's private Docker network address.

```sh
python3 deploy/release.py stage --root /private/deployment-root \
  --release /private/release-directory --api-image local/cpa-bps-rebuilt-api:0.5.11
python3 deploy/release.py apply --root /private/deployment-root \
  --release /private/release-directory --api-image local/cpa-bps-rebuilt-api:0.5.11 \
  --manager-image local/cpa-bps-rebuilt-manager:0.5.11
```

The stage uses private credential copies on an internal Docker network with no
external route, so no provider can rotate live refresh tokens concurrently.
Its model registration uses a snapshot of the existing BPS catalog; live model
discovery is verified separately after cutover. No generation request is required
by stage verification. Apply refuses active BPS requests, stops Manager/API before
backing up their persistent state, converts the latest credentials, then starts
and verifies the new services. Only the two CPA containers are touched. No other
host service or ingress configuration is changed by this script. If stopping or
creating the pre-migration snapshot fails, the unchanged original services are
restarted without attempting to restore an incomplete backup.

The routing policy uses 24-hour session affinity, automatic rebind and no extra
retry rounds. Every eligible credential can be considered once; exhausted
accounts enter account-wide cooldown. Quality schedules start empty on a new
installation, not copied from a developer's test accounts.

## Rollback

The deployment result records its private backup directory. If post-cutover
verification fails, rollback runs automatically. To roll back later:

```sh
python3 deploy/release.py rollback --root /private/deployment-root \
  --backup /private/deployment-root/backups/before-rebuilt-TIMESTAMP
```

Rollback restores the credential format used by the snapshot: legacy top-level
tokens for an original plugin, or `oauth_grant` for a previous reconstructed
release. Never blindly restore an older refresh token: OAuth may already have
rotated it. Current mutable flags and token values take precedence over the
metadata snapshot. Accounts added since the snapshot keep their own metadata
and use its unambiguous target format; ambiguous snapshots stop for recovery.
Old config/Compose and Manager data are restored; post-failure Manager data are
kept separately. Previous image tags remain available.

If reverse credential conversion fails, the script stops and preserves the
current credentials for manual recovery rather than replacing them with stale
secrets. Do not delete backups until the upgrade has been accepted.

## Verify after an upgrade

- Both containers are healthy, the expected plugin version is registered.
- Credential IDs, auth indexes, disabled flags and concurrency limits match.
- Public `/v1/models`, `/v1/files` and Manager/plugin pages respond correctly.
- No-key inference/Files requests remain unauthorized.
- Account email and plan render; authorization status is below the workflow.
- Use a short, synthetic real request only on an authorized account with quota.
  Stop on 401/403/429 and retain the failure. Do not consume reset cards as a test.
- Inspect Manager usage collection and preserve its original database/key.
