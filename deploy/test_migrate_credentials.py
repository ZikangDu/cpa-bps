#!/usr/bin/env python3
"""Synthetic-only migration tests. No real credentials or network are used."""
import json
import os
from pathlib import Path
import tempfile
import unittest

from migrate_credentials import BPS_CLIENT_ID, BPS_TOKEN_URL, MigrationError, convert, stage


def fixture():
    return {"type":"cpa-bps","access_token":"fixture-access","refresh_token":"fixture-refresh",
            "expires_at":"2030-01-02T03:04:05Z","expired":"2030-01-02T03:04:05Z",
            "disabled":True,"email":"fixture@example.invalid","chatgpt_account_id":"fixture-account",
            "user_id":"fixture-user","priority":-2,"note":"operator note","extra":{"keep":True}}


class MigrationTests(unittest.TestCase):
    def test_forward_preserves_identity_and_removes_duplicate_tokens(self):
        old=fixture();new,action=convert(old)
        self.assertEqual(action,"to_reconstructed")
        self.assertNotIn("access_token",new);self.assertNotIn("refresh_token",new)
        self.assertEqual(new["oauth_grant"]["access_token"],old["access_token"])
        self.assertEqual(new["oauth_grant"]["client_id"],BPS_CLIENT_ID)
        self.assertEqual(new["oauth_grant"]["token_url"],BPS_TOKEN_URL)
        for key in ("disabled","email","chatgpt_account_id","user_id","priority","note","extra"):
            self.assertEqual(new[key],old[key])
        self.assertIn("access_token",old)
        self.assertEqual(convert(new),(new,"already_reconstructed"))

    def test_rollback_prefers_rotated_current_tokens_and_current_flags(self):
        old=fixture();new,_=convert(old)
        new["oauth_grant"]["access_token"]="fixture-rotated-access"
        new["oauth_grant"]["refresh_token"]="fixture-rotated-refresh"
        new["oauth_grant"]["expires_at"]="2030-01-03T03:04:05Z"
        new["disabled"]=False;new["priority"]=7
        del new["note"]
        restored,action=convert(new,True,old)
        self.assertEqual(action,"to_original")
        self.assertEqual(restored["access_token"],"fixture-rotated-access")
        self.assertEqual(restored["refresh_token"],"fixture-rotated-refresh")
        self.assertEqual(restored["expired"],"2030-01-03T03:04:05Z")
        self.assertFalse(restored["disabled"]);self.assertEqual(restored["priority"],7)
        self.assertEqual(restored["note"],old["note"])
        self.assertNotIn("oauth_grant",restored)
        self.assertNotIn("auth_id",restored)
        self.assertNotIn("label",restored)

    def test_explicit_host_identity_preserves_non_filename_auth_id(self):
        old=fixture()
        identity={"auth_id":"cpa-bps-original-hash","label":"Existing BPS account","concurrency_limit":0}
        new,_=convert(old,identity=identity)
        self.assertEqual(new["auth_id"],identity["auth_id"])
        self.assertEqual(new["label"],identity["label"])
        self.assertNotIn("concurrency_limit",new)
        restored,_=convert(new,True,old)
        self.assertNotIn("auth_id",restored)
        for invalid in [{"auth_id":""},{"auth_id":"../bad"},{"auth_id":"valid","concurrency_limit":True}]:
            with self.assertRaises(MigrationError):convert(old,identity=invalid)

    def test_missing_rotated_refresh_never_restores_snapshot_secret(self):
        old=fixture();new,_=convert(old);del new["oauth_grant"]["refresh_token"]
        restored,_=convert(new,True,old)
        self.assertNotIn("refresh_token",restored)

    def test_modern_rollback_preserves_layout_current_grant_and_stable_metadata(self):
        old,_=convert(fixture(),identity={"auth_id":"cpa-bps-stable","label":"Stable label"})
        old["oauth_grant"]["id_token"]="fixture-old-id"
        new=json.loads(json.dumps(old))
        new["oauth_grant"].update(access_token="fixture-new-access",refresh_token="fixture-new-refresh",
                                expires_at="2030-01-04T03:04:05Z",scope="openid profile")
        del new["oauth_grant"]["id_token"]
        new.update(disabled=False,priority=9,extra={"updated":True})
        del new["note"]
        unchanged=json.loads(json.dumps(new))
        restored,action=convert(new,True,old)
        self.assertEqual(action,"restored_reconstructed")
        self.assertEqual(restored["oauth_grant"],new["oauth_grant"])
        for field in ("auth_id","label","email","disabled","priority","extra"):
            self.assertEqual(restored[field],new[field])
        self.assertEqual(restored["note"],old["note"])
        self.assertNotIn("access_token",restored);self.assertNotIn("expired",restored)
        self.assertEqual(new,unchanged)
        del new["oauth_grant"]["refresh_token"]
        restored,_=convert(new,True,old)
        self.assertNotIn("refresh_token",restored["oauth_grant"])

    def test_modern_snapshot_can_restore_current_legacy_tokens_without_stale_duplicates(self):
        old,_=convert(fixture(),identity={"auth_id":"cpa-bps-stable","label":"Stable label"})
        current=fixture();current.update(access_token="fixture-latest-access",refresh_token="fixture-latest-refresh",disabled=False)
        restored,_=convert(current,True,old)
        self.assertEqual(restored["auth_id"],old["auth_id"])
        self.assertEqual(restored["label"],old["label"])
        self.assertEqual(restored["oauth_grant"]["refresh_token"],"fixture-latest-refresh")
        self.assertFalse(restored["disabled"])
        for name in ("access_token","refresh_token","expires_at","expired"):
            self.assertNotIn(name,restored)
        corrupt=json.loads(json.dumps(old));corrupt["oauth_grant"]["client_id"]="wrong-registration"
        with self.assertRaises(MigrationError):convert(current,True,corrupt)

    def test_forward_staging_modern_credentials_is_byte_preserving_and_idempotent(self):
        with tempfile.TemporaryDirectory() as work:
            root=Path(work);source=root/'source';source.mkdir()
            identity={"auth_id":"cpa-bps-stable","label":"Stable label","concurrency_limit":4}
            current,_=convert(fixture(),identity=identity)
            current["oauth_grant"]["future_grant_metadata"]={"keep":True}
            raw=(json.dumps(current,indent=3)+'\n').encode()
            (source/'same.json').write_bytes(raw)
            for index in range(2):
                target=root/('copy'+str(index))
                report=stage(source,target,apply=True,identity_map={"same.json":identity})
                self.assertTrue(report['applied']);self.assertEqual(report['unchanged'],1)
                self.assertEqual((target/'same.json').read_bytes(),raw)
                source=target

    def test_rollback_stages_each_snapshot_format_with_latest_tokens(self):
        with tempfile.TemporaryDirectory() as work:
            root=Path(work);source=root/'source';source.mkdir();snapshot=root/'snapshot';snapshot.mkdir()
            for name,modern in [('legacy.json',False),('modern.json',True)]:
                original=fixture()
                if modern:original,_=convert(original,identity={"auth_id":"cpa-bps-stable"})
                (snapshot/name).write_text(json.dumps(original))
                current,_=convert(original)
                current["oauth_grant"]["refresh_token"]="fixture-latest-refresh"
                (source/name).write_text(json.dumps(current))
            report=stage(source,root/'rollback',apply=True,to_original=True,snapshot=snapshot)
            self.assertTrue(report['applied']);self.assertFalse(report['errors'])
            legacy=json.loads((root/'rollback/legacy.json').read_text())
            modern=json.loads((root/'rollback/modern.json').read_text())
            self.assertNotIn('oauth_grant',legacy)
            self.assertEqual(legacy['refresh_token'],'fixture-latest-refresh')
            self.assertEqual(modern['oauth_grant']['refresh_token'],'fixture-latest-refresh')
            self.assertEqual(modern['auth_id'],'cpa-bps-stable')
            self.assertNotIn('refresh_token',modern)

    def test_new_account_rollback_uses_snapshot_format_without_copying_other_account(self):
        for modern_snapshot in (False,True):
            with self.subTest(modern_snapshot=modern_snapshot), tempfile.TemporaryDirectory() as work:
                root=Path(work);source=root/'source';source.mkdir();snapshot=root/'snapshot';snapshot.mkdir()
                old=fixture();old['snapshot_only']='must-not-copy'
                if modern_snapshot:old,_=convert(old,identity={'auth_id':'cpa-bps-old'})
                (snapshot/'old.json').write_text(json.dumps(old))
                new=fixture();new.update(email='new@example.invalid',note='new account')
                new,_=convert(new,identity={'auth_id':'cpa-bps-new','label':'New account'})
                new['oauth_grant']['refresh_token']='fixture-new-account-refresh'
                (source/'new.json').write_text(json.dumps(new))
                report=stage(source,root/'rollback',apply=True,to_original=True,snapshot=snapshot)
                self.assertTrue(report['applied'])
                restored=json.loads((root/'rollback/new.json').read_text())
                self.assertEqual('oauth_grant' in restored,modern_snapshot)
                self.assertEqual(restored['email'],'new@example.invalid')
                self.assertNotIn('snapshot_only',restored)
                grant=restored.get('oauth_grant',restored)
                self.assertEqual(grant['refresh_token'],'fixture-new-account-refresh')
                if modern_snapshot:self.assertEqual(restored['auth_id'],'cpa-bps-new')

    def test_new_account_with_ambiguous_snapshot_formats_stops_before_writing(self):
        with tempfile.TemporaryDirectory() as work:
            root=Path(work);source=root/'source';source.mkdir();snapshot=root/'snapshot';snapshot.mkdir()
            modern,_=convert(fixture())
            (snapshot/'legacy.json').write_text(json.dumps(fixture()))
            (snapshot/'modern.json').write_text(json.dumps(modern))
            (source/'new.json').write_text(json.dumps(modern))
            report=stage(source,root/'rollback',apply=True,to_original=True,snapshot=snapshot)
            self.assertFalse(report['applied']);self.assertFalse((root/'rollback').exists())
            self.assertEqual(report['errors'][0]['code'],'snapshot_format_unknown_for_new_credential')

    def test_expiry_exact_timezone_and_no_invented_lifetime(self):
        for value in [None,0,"0","", "0001-01-01T00:00:00Z","2030-01-02T03:04:05","not-a-time"]:
            old=fixture();old["expires_at"]=value
            with self.assertRaises(MigrationError):convert(old)
        old=fixture();old.pop("expires_at");old["expired"]="2020-01-02T11:04:05+08:00"
        new,_=convert(old)
        self.assertEqual(new["oauth_grant"]["expires_at"],"2020-01-02T03:04:05Z")
        self.assertTrue(new["disabled"])

    def test_invalid_authorization_never_enables(self):
        for patch in [{"disabled":"true"},{"client_id":"another-registration"},{"access_token":""},{"upstream_origin":"https://other.invalid"}]:
            old=fixture();old.update(patch)
            with self.assertRaises(MigrationError):convert(old)
            self.assertNotIn("oauth_grant",old)
        new,_=convert(fixture());new["oauth_grant"]["token_url"]="https://other.invalid/token"
        with self.assertRaises(MigrationError):convert(new,True)

    def test_private_atomic_stage_and_dry_run(self):
        with tempfile.TemporaryDirectory() as work:
            root=Path(work);source=root/'source';source.mkdir();sub=source/'nested';sub.mkdir()
            original=(json.dumps(fixture())+'\n').encode();(sub/'same-name.json').write_bytes(original)
            other=b'{"type":"codex","disabled":true,"note":"preserve bytes"}\n';(source/'other.json').write_bytes(other)
            target=root/'copy'
            report=stage(source,target);self.assertTrue(report['dry_run']);self.assertFalse(target.exists())
            report=stage(source,target,apply=True);self.assertTrue(report['applied']);self.assertEqual(report['converted'],1);self.assertEqual(report['other_provider'],1)
            self.assertEqual((sub/'same-name.json').read_bytes(),original)
            self.assertEqual((target/'other.json').read_bytes(),other)
            self.assertEqual(os.stat(target/'nested/same-name.json').st_mode&0o777,0o600)
            self.assertEqual(os.stat(target).st_mode&0o777,0o700)
            output=json.dumps(report);self.assertNotIn('fixture-access',output);self.assertNotIn('fixture@example',output);self.assertNotIn('same-name',output)
            with self.assertRaises(MigrationError):stage(source,source,apply=True)
            with self.assertRaises(MigrationError):stage(source,target,apply=True)

    def test_invalid_batch_is_not_partially_written_and_reports_no_secret(self):
        with tempfile.TemporaryDirectory() as work:
            root=Path(work);source=root/'source';source.mkdir();target=root/'copy'
            (source/'valid.json').write_text(json.dumps(fixture()))
            bad=fixture();bad['expires_at']='private-invalid-expiry';(source/'private-email@example.invalid.json').write_text(json.dumps(bad))
            report=stage(source,target,apply=True)
            self.assertFalse(report['applied']);self.assertFalse(target.exists());self.assertEqual(len(report['errors']),1)
            self.assertNotIn('private-',json.dumps(report))

    def test_symlink_refused(self):
        with tempfile.TemporaryDirectory() as work:
            root=Path(work);source=root/'source';source.mkdir();target=root/'copy';outside=root/'outside.json';outside.write_text(json.dumps(fixture()))
            (source/'linked.json').symlink_to(outside)
            report=stage(source,target,apply=True)
            self.assertFalse(report['applied']);self.assertEqual(report['errors'][0]['code'],'invalid_credential_file')


if __name__=='__main__':unittest.main()
