#!/usr/bin/env python3
"""Stage a private, reversible copy of BPS credentials without contacting OAuth.

Dry-run is the default. --apply always requires a separate empty output directory;
this program never overwrites a running auth directory. File names and relative
paths remain unchanged. Reports include counts and name digests, never tokens or
email addresses. Keep the pre-migration snapshot private for rollback metadata.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import tempfile
from datetime import datetime, timezone

BPS_CLIENT_ID = "app_fnr0pYvVwwFDocDumLG3H2Bp"
BPS_TOKEN_URL = "https://auth.openai.com/oauth/token?unified=true"
BPS_ORIGIN = "https://bps.openai.com"
MAX_CREDENTIAL_BYTES = 2 << 20
TOKEN_FIELDS = ("access_token", "refresh_token", "id_token")
GRANT_FIELDS = (*TOKEN_FIELDS, "expires_at", "scope", "earliest_refresh_at")


class MigrationError(ValueError):
    """The message is a fixed public category, never source data."""


def valid_text(value, required=False):
    if value is None and not required:
        return None
    if not isinstance(value, str) or (required and not value) or len(value) > 1 << 20:
        raise MigrationError("invalid_token_field")
    if any(ord(c) <= 32 or ord(c) >= 127 for c in value):
        raise MigrationError("invalid_token_field")
    return value


def timestamp(value):
    if not isinstance(value, str) or not value or len(value) > 64:
        raise MigrationError("invalid_expiry")
    try:
        parsed = datetime.fromisoformat(value[:-1] + "+00:00" if value.endswith("Z") else value)
    except (ValueError, OverflowError):
        raise MigrationError("invalid_expiry") from None
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        raise MigrationError("expiry_timezone_required")
    try:
        parsed = parsed.astimezone(timezone.utc)
    except (ValueError, OverflowError):
        raise MigrationError("invalid_expiry") from None
    if parsed <= datetime(1970, 1, 1, tzinfo=timezone.utc):
        raise MigrationError("invalid_expiry")
    return parsed.isoformat().replace("+00:00", "Z")


def expiry_from(record):
    if "expires_at" in record:
        return timestamp(record["expires_at"])
    if "expired" in record:
        return timestamp(record["expired"])
    raise MigrationError("missing_expiry")


def validate_common(record):
    if not isinstance(record, dict):
        raise MigrationError("invalid_credential_object")
    if "disabled" in record and not isinstance(record["disabled"], bool):
        raise MigrationError("invalid_disabled_flag")


def validate_identity_map(value):
    if not isinstance(value, dict) or len(value) > 10000:
        raise MigrationError("invalid_identity_map")
    for filename, entry in value.items():
        if (not isinstance(filename, str) or not filename.endswith(".json")
                or Path(filename).is_absolute() or ".." in Path(filename).parts
                or "\\" in filename or any(ord(c) < 32 for c in filename)):
            raise MigrationError("invalid_identity_map")
        if not isinstance(entry, dict) or set(entry) - {"auth_id", "label", "concurrency_limit"}:
            raise MigrationError("invalid_identity_map")
        identity = entry.get("auth_id")
        if (not isinstance(identity, str) or not 0 < len(identity) <= 1024
                or any(not (c.isascii() and (c.isalnum() or c in "_.:-")) for c in identity)):
            raise MigrationError("invalid_identity_map")
        if "label" in entry and (not isinstance(entry["label"], str) or len(entry["label"]) > 1024
                                  or any(ord(c) < 32 for c in entry["label"])):
            raise MigrationError("invalid_identity_map")
        if "concurrency_limit" in entry and (type(entry["concurrency_limit"]) is not int
                                             or not 0 <= entry["concurrency_limit"] <= 10000):
            raise MigrationError("invalid_identity_map")
    return value


def validate_grant(grant):
    if not isinstance(grant, dict):
        raise MigrationError("invalid_oauth_grant")
    if grant.get("client_id") != BPS_CLIENT_ID or grant.get("token_url") != BPS_TOKEN_URL:
        raise MigrationError("oauth_registration_mismatch")
    valid_text(grant.get("access_token"), True)
    for name in ("refresh_token", "id_token"):
        if name in grant:
            valid_text(grant[name])
    timestamp(grant.get("expires_at"))
    if grant.get("earliest_refresh_at"):
        timestamp(grant["earliest_refresh_at"])
    if "scope" in grant and (not isinstance(grant["scope"], str) or len(grant["scope"]) > 4096):
        raise MigrationError("invalid_scope")


def convert(record, to_original=False, snapshot=None, identity=None):
    """Return (converted record, action); never mutate the source object.

    Rollback restores the snapshot's storage format. Without a snapshot it
    retains the historical --to-original behavior and emits legacy fields.
    """
    validate_common(record)
    if record.get("type") != "cpa-bps":
        return dict(record), "other_provider"
    result = dict(record)
    if not to_original:
        if identity is not None:
            validate_identity_map({"credential.json": identity})
        if "oauth_grant" in record:
            validate_grant(record["oauth_grant"])
            if record.get("upstream_origin") != BPS_ORIGIN or record.get("token_env"):
                raise MigrationError("credential_origin_mismatch")
            if identity is not None:
                result["auth_id"] = identity["auth_id"]
                if "label" in identity:
                    result["label"] = identity["label"]
            return result, "already_reconstructed" if result == record else "identity_updated"
        if record.get("token_env"):
            raise MigrationError("token_environment_credential_requires_review")
        if record.get("client_id", BPS_CLIENT_ID) != BPS_CLIENT_ID:
            raise MigrationError("oauth_registration_mismatch")
        if record.get("token_url", BPS_TOKEN_URL) != BPS_TOKEN_URL:
            raise MigrationError("oauth_registration_mismatch")
        if record.get("upstream_origin", BPS_ORIGIN) != BPS_ORIGIN:
            raise MigrationError("credential_origin_mismatch")
        grant = {"client_id": BPS_CLIENT_ID, "token_url": BPS_TOKEN_URL,
                 "access_token": valid_text(record.get("access_token"), True),
                 "expires_at": expiry_from(record)}
        for name in ("refresh_token", "id_token"):
            if name in record:
                grant[name] = valid_text(record[name])
        if "scope" in record:
            grant["scope"] = record["scope"]
        if record.get("earliest_refresh_at"):
            grant["earliest_refresh_at"] = timestamp(record["earliest_refresh_at"])
        validate_grant(grant)
        # One token source prevents an old top-level secret from shadowing a
        # rotated grant when CPA merges provider storage and runtime metadata.
        for name in (*GRANT_FIELDS, "expired", "client_id", "token_url"):
            result.pop(name, None)
        result["upstream_origin"] = BPS_ORIGIN
        result["oauth_grant"] = grant
        result.setdefault("label", "BPS OAuth")
        if identity is not None:
            result["auth_id"] = identity["auth_id"]
            if "label" in identity:
                result["label"] = identity["label"]
        return result, "to_reconstructed"
    if snapshot is not None:
        validate_common(snapshot)
        if snapshot.get("type") != "cpa-bps":
            raise MigrationError("snapshot_provider_mismatch")
        if "oauth_grant" in snapshot:
            # A later reconstructed-to-reconstructed upgrade must not turn its
            # rollback credentials into a format the previous image cannot read.
            # Validate both records, but take the entire current grant so missing
            # rotated fields cannot be resurrected from the snapshot.
            convert(snapshot)
            current, _ = convert(record)
            result = dict(snapshot)
            result.update(record)
            for name in (*GRANT_FIELDS, "expired", "client_id", "token_url", "token_env"):
                result.pop(name, None)
            result["oauth_grant"] = current["oauth_grant"]
            result["upstream_origin"] = BPS_ORIGIN
            result.setdefault("label", current.get("label", "BPS OAuth"))
            return result, "already_reconstructed" if result == record else "restored_reconstructed"
    if "oauth_grant" not in record:
        valid_text(record.get("access_token"), True)
        expiry_from(record)
        return result, "already_original"
    grant = record["oauth_grant"]
    validate_grant(grant)
    if record.get("upstream_origin") != BPS_ORIGIN or record.get("token_env"):
        raise MigrationError("credential_origin_mismatch")
    if snapshot is not None:
        result = dict(snapshot)
        result.update(record)
    # Current mutable metadata wins over the snapshot. Current grant tokens
    # always win over pre-deployment tokens, including after refresh rotation.
    for name in (*TOKEN_FIELDS, "oauth_grant", "auth_id", "label", "token_env", "upstream_origin",
                 "expires_at", "expired", "scope", "earliest_refresh_at"):
        result.pop(name, None)
    for name in TOKEN_FIELDS:
        if name in grant:
            result[name] = grant[name]
    result["expires_at"] = timestamp(grant["expires_at"])
    result["expired"] = result["expires_at"]
    for name in ("scope", "earliest_refresh_at"):
        if name in grant:
            result[name] = grant[name]
    return result, "to_original"


def read_object(path):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > MAX_CREDENTIAL_BYTES:
        raise MigrationError("invalid_credential_file")
    try:
        raw = path.read_bytes()
        value = json.loads(raw)
    except (OSError, ValueError):
        raise MigrationError("invalid_credential_json") from None
    validate_common(value)
    return raw, value


def stage(source: Path, output: Path | None = None, *, apply=False, to_original=False,
          snapshot: Path | None = None, identity_map=None):
    if source.is_symlink() or not source.is_dir():
        raise MigrationError("invalid_source_directory")
    source = source.resolve()
    identities = validate_identity_map(identity_map) if identity_map is not None else {}
    snapshot_formats = set()
    if snapshot is not None:
        if snapshot.is_symlink() or not snapshot.is_dir():
            raise MigrationError("invalid_snapshot_directory")
        snapshot = snapshot.resolve()
        if to_original:
            # Accounts may be added after deployment. If their file is absent
            # from the snapshot, infer only the storage format, never copy
            # another account's metadata or secrets as a template.
            for directory, dirs, files in os.walk(snapshot, followlinks=False):
                for name in dirs:
                    if (Path(directory) / name).is_symlink():
                        raise MigrationError("symlink_in_snapshot_directory")
                for name in files:
                    if name.endswith(".json"):
                        _, prior = read_object(Path(directory) / name)
                        if prior.get("type") == "cpa-bps":
                            snapshot_formats.add("reconstructed" if "oauth_grant" in prior else "original")
    if apply:
        if output is None:
            raise MigrationError("apply_requires_output_directory")
        if output.is_symlink():
            raise MigrationError("invalid_output_directory")
        output = output.resolve()
        if output == source or source in output.parents or output in source.parents:
            raise MigrationError("output_must_be_separate")
        if output.exists() and (not output.is_dir() or any(output.iterdir())):
            raise MigrationError("output_must_be_empty")
    report = {"mode": "to_original" if to_original else "to_reconstructed",
              "dry_run": not apply, "scanned": 0, "converted": 0,
              "unchanged": 0, "other_provider": 0, "expired_grants": 0,
              "disabled_bps": 0, "identity_mapped": 0, "errors": [], "applied": False}
    prepared = []
    for directory, dirs, files in os.walk(source, followlinks=False):
        for name in dirs:
            if (Path(directory) / name).is_symlink():
                raise MigrationError("symlink_in_source_directory")
        for name in sorted(files):
            path = Path(directory) / name
            if not name.endswith(".json"):
                continue
            relative = path.relative_to(source)
            report["scanned"] += 1
            digest = hashlib.sha256(str(relative).encode()).hexdigest()[:12]
            try:
                raw, current = read_object(path)
                old = None
                if snapshot is not None and (snapshot / relative).exists():
                    _, old = read_object(snapshot / relative)
                identity = identities.get(relative.as_posix()) if not to_original else None
                if to_original and snapshot is not None and old is None and current.get("type") == "cpa-bps":
                    if snapshot_formats == {"reconstructed"}:
                        updated, action = convert(current)
                    elif snapshot_formats == {"original"}:
                        updated, action = convert(current, True)
                    else:
                        raise MigrationError("snapshot_format_unknown_for_new_credential")
                else:
                    updated, action = convert(current, to_original, old, identity)
                if action == "other_provider":
                    report["other_provider"] += 1
                elif action.startswith("already_"):
                    report["unchanged"] += 1
                else:
                    report["converted"] += 1
                if current.get("type") == "cpa-bps":
                    report["identity_mapped"] += identity is not None
                    report["disabled_bps"] += current.get("disabled") is True
                    grant = updated.get("oauth_grant", updated)
                    if datetime.fromisoformat(expiry_from(grant).replace("Z", "+00:00")) < datetime.now(timezone.utc):
                        report["expired_grants"] += 1
                payload = raw if action in ("other_provider", "already_original", "already_reconstructed") else (json.dumps(updated, ensure_ascii=False, indent=2) + "\n").encode()
                prepared.append((relative, payload))
            except MigrationError as exc:
                report["errors"].append({"file_digest": digest, "code": str(exc)})
    if apply and not report["errors"]:
        output.parent.mkdir(parents=True, exist_ok=True)
        temp = Path(tempfile.mkdtemp(prefix=".credential-migration-", dir=output.parent))
        try:
            for relative, payload in prepared:
                destination = temp / relative
                destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                descriptor = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                with os.fdopen(descriptor, "wb") as file:
                    file.write(payload)
                    file.flush()
                    os.fsync(file.fileno())
            if output.exists():
                output.rmdir()
            os.replace(temp, output)
            report["applied"] = True
        finally:
            if temp.exists():
                shutil.rmtree(temp)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("auth_dir", type=Path)
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--output-dir", type=Path)
    parser.add_argument("--to-original", action="store_true")
    parser.add_argument("--snapshot", type=Path, help="private credential snapshot selecting rollback format and metadata")
    parser.add_argument("--identity-map", type=Path,
                        help="private filename-to-auth_id/label/concurrency_limit map from the old host")
    args = parser.parse_args()
    try:
        identities = None
        if args.identity_map is not None:
            _, identities = read_object(args.identity_map)
        report = stage(args.auth_dir, args.output_dir, apply=args.apply,
                       to_original=args.to_original, snapshot=args.snapshot, identity_map=identities)
    except (MigrationError, OSError) as exc:
        code = str(exc) if isinstance(exc, MigrationError) else "filesystem_operation_failed"
        print(json.dumps({"applied": False, "error": code}))
        return 2
    print(json.dumps(report, ensure_ascii=False))
    return 1 if report["errors"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
