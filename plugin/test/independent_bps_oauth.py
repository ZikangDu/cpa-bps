#!/usr/bin/env python3
"""Verify the independent BPS login preset without exchanging a live token."""

import argparse
import http.server
import json
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import tempfile
import threading
from urllib.parse import parse_qs, urlencode, urlsplit

from full_integration import call, start_session
from integration import LOOPBACK, read_diagnostics, require, reserve_port
from live_responses import DenyProxy


# Public OAuth client and callback taken from the bps-local-public reference.
CLIENT_ID = "app_fnr0pYvVwwFDocDumLG3H2Bp"
REDIRECT_URI = (
    "https://bps.openai.com/basispoints/extension/"
    "360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback"
)
REQUIRED_SCOPES = {"openid", "offline_access", "email", "profile", "organization.read"}
MANAGEMENT = "/v8/management"


def inspect_login(process, port, key, auths):
    login = start_session(process, port, key)
    authorize = urlsplit(login["url"])
    query = parse_qs(authorize.query)
    require(
        authorize.scheme == "https"
        and authorize.netloc == "auth.openai.com"
        and authorize.path in {"/oauth/authorize", "/api/accounts/authorize"}
        and not authorize.fragment,
        "BPS preset did not provide the reference authorization origin and route",
    )
    expected = {
        "client_id": CLIENT_ID,
        "redirect_uri": REDIRECT_URI,
        "response_type": "code",
        "code_challenge_method": "S256",
        "state": login["state"],
    }
    require(all(query.get(name) == [value] for name, value in expected.items()),
            "BPS authorization parameters do not match the preset")
    require(re.fullmatch(r"[A-Za-z0-9_-]{43}", login["state"]) is not None,
            "BPS login state is not a fresh opaque nonce")
    challenge = query.get("code_challenge", [])
    require(len(challenge) == 1 and re.fullmatch(r"[A-Za-z0-9_-]{43}", challenge[0]) is not None,
            "BPS preset did not generate a valid PKCE challenge")
    require(len(query.get("scope", [])) == 1 and set(query["scope"][0].split()) == REQUIRED_SCOPES,
            "BPS scopes do not match the reference login contract")
    require("code_verifier" not in query and "client_secret" not in query,
            "BPS authorization URL exposed token-exchange material")

    status, headers, body = call(port, "GET", "/v0/resource/plugins/cpa-bps/index.html")
    require(status == 200 and "text/html" in headers.get("Content-Type", "")
            and b'data-oauth-ready="true"' in body,
            "BPS authorization page was not ready after preset registration")

    unknown = secrets.token_urlsafe(32)
    status, _, _ = call(
        port, "POST", MANAGEMENT + "/oauth/callback",
        data={"provider": "cpa-bps", "state": unknown, "code": "synthetic-unknown-code"},
    )
    require(status == 404, "CPA accepted a callback for an unknown BPS state")
    status, _, _ = call(
        port, "POST", MANAGEMENT + "/oauth/callback",
        data={"provider": "another-provider", "state": login["state"], "code": "synthetic-wrong-provider"},
    )
    require(status == 400, "CPA accepted a callback with a mismatched provider")

    status, _, body = call(
        port, "GET", MANAGEMENT + "/oauth/status?" + urlencode({"state": login["state"]}), key=key,
    )
    require(status == 200 and json.loads(body).get("status") == "wait",
            "Rejected callbacks changed the pending BPS authorization")
    status, _, body = call(port, "GET", MANAGEMENT + "/credentials", key=key)
    require(status == 200 and json.loads(body).get("files") == [],
            "Rejected callbacks created a saved OAuth credential")
    require(not list(auths.glob(".oauth-*.oauth")), "Rejected callbacks persisted an OAuth callback")
    print("PASS isolated CPA: BPS login preset, PKCE, authorization page, and callback-state rejection")


def run(cpa, plugin):
    with tempfile.TemporaryDirectory(prefix="cpa-independent-bps-") as workdir:
        root = Path(workdir)
        auths, plugins = root / "auths", root / "plugins"
        auths.mkdir(mode=0o700)
        plugins.mkdir(mode=0o700)
        shutil.copy2(plugin, plugins / ("cpa-bps" + plugin.suffix))
        port = reserve_port()
        key = secrets.token_urlsafe(24)
        config = root / "config.yaml"
        config.write_text(json.dumps({
            "host": LOOPBACK,
            "port": port,
            "auth-dir": str(auths),
            "api-keys": ["independent-bps-fixture-key"],
            "management": {
                "secret-key": key,
                "disable-control-panel": True,
                "disable-auto-update-panel": True,
            },
            "plugins": {"enabled": True, "dir": str(plugins), "configs": {"cpa-bps": {
                "enabled": True,
                "oauth_preset": "bps",
                "responses_url": "https://bps.openai.com/basispoints/api/responses",
                "models": ["gpt-6.2-sol"],
                "account_concurrency_limit": 1,
            }}},
        }))
        with http.server.ThreadingHTTPServer((LOOPBACK, 0), DenyProxy) as deny:
            thread = threading.Thread(target=deny.serve_forever, daemon=True)
            thread.start()
            proxy = f"http://{LOOPBACK}:{deny.server_port}"
            env = {
                "XDG_CONFIG_HOME": workdir,
                "XDG_CACHE_HOME": workdir,
                "TMPDIR": workdir,
                "HTTP_PROXY": proxy,
                "HTTPS_PROXY": proxy,
                "NO_PROXY": LOOPBACK,
            }
            log_path = root / "cpa.log"
            try:
                with log_path.open("wb") as log:
                    process = subprocess.Popen(
                        [str(cpa), "-local-model", "-config", str(config)],
                        cwd=root, env=env, stdout=log, stderr=log,
                    )
                    try:
                        inspect_login(process, port, key, auths)
                    except Exception as error:
                        log.flush()
                        raise RuntimeError(f"{error}\nCPA diagnostics:\n{read_diagnostics(log_path)}") from error
                    finally:
                        if process.poll() is None:
                            process.terminate()
                        try:
                            process.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait(timeout=5)
            finally:
                deny.shutdown()
                thread.join(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", required=True, type=Path)
    parser.add_argument("--plugin", required=True, type=Path)
    args = parser.parse_args()
    run(args.cpa.resolve(strict=True), args.plugin.resolve(strict=True))


if __name__ == "__main__":
    main()
