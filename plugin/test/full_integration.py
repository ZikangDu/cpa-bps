#!/usr/bin/env python3
"""Verify the main native plugin flows against a synthetic loopback issuer and API."""

import argparse
import base64
from contextlib import closing, contextmanager
from email import policy
from email.parser import BytesParser
import hashlib
import http.client
import http.server
import json
import pathlib
import re
import secrets
import shutil
import subprocess
import tempfile
import threading
import time
from urllib.parse import parse_qs, urlencode, urlsplit

from integration import (
    API_KEY, LOOPBACK, MARKER, MODEL, RESPONSE_BODY, UPSTREAM_MODEL, Upstream,
    event_bytes, read_diagnostics, request, require, reserve_port, start_ready,
)


MANAGEMENT = "/v8/management"
CALLBACK = MANAGEMENT + "/oauth/callback"
CLIENT_ID = "fixture-client"
FIRST_ACCESS = "fixture-access-first"
SECOND_ACCESS = "fixture-access-second"
FIRST_REFRESH = "fixture-refresh-first"
SECOND_REFRESH = "fixture-refresh-second"
FILE_ID = "file-fixture"
PNG_BASE64 = (
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aAq0AAAAASUVORK5CYII="
)
PNG = base64.b64decode(PNG_BASE64)
DECLARED_TOOL = "fixture_lookup"
TOOL = {"type": "function", "name": DECLARED_TOOL, "parameters": {"type": "object"}}


class Fixture(Upstream):
    def json_response(self, status, value):
        self.reply(status, json.dumps(value).encode())

    def do_GET(self):
        url = urlsplit(self.path)
        if url.path == "/authorize":
            self.authorize(parse_qs(url.query, keep_blank_values=True))
        elif url.path == "/v1/quota":
            self.quota()
        else:
            self.json_response(404, {})

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path == "/token":
            self.token(parse_qs(body.decode(), keep_blank_values=True))
            return
        if not self.authorized():
            self.json_response(401, {"error": "unexpected credential"})
            return
        if self.path == "/v1/files":
            self.upload(body)
        elif self.path == "/v1/responses":
            self.generate(json.loads(body))
        else:
            self.json_response(404, {})

    def do_DELETE(self):
        with self.server.lock:
            self.server.upstream_deletions += 1
        self.json_response(405, {})

    def authorized(self):
        with self.server.lock:
            return self.headers.get("Authorization") == "Bearer " + self.server.access

    def authorize(self, query):
        fields = {
            "client_id": CLIENT_ID, "redirect_uri": self.server.redirect_uri, "response_type": "code",
            "scope": "responses", "code_challenge_method": "S256",
        }
        for key, value in fields.items():
            if query.get(key) != [value]:
                self.json_response(400, {"error": f"unexpected fixture {key}"})
                return
        state, challenge = query.get("state", [""])[0], query.get("code_challenge", [""])[0]
        if not re.fullmatch(r"[A-Za-z0-9_-]{32,128}", state) or not re.fullmatch(r"[A-Za-z0-9_-]{43}", challenge):
            self.json_response(400, {"error": "invalid fixture state or challenge"})
            return
        code = secrets.token_urlsafe(24)
        with self.server.lock:
            self.server.codes[code] = challenge
        self.send_response(302)
        self.send_header("Location", self.server.redirect_uri + "?" + urlencode({"state": state, "code": code}))
        self.send_header("Content-Length", "0")
        self.end_headers()

    def token(self, form):
        if form.get("client_id") != [CLIENT_ID]:
            self.json_response(400, {"error": "invalid_client"})
            return
        with self.server.lock:
            grant = form.get("grant_type", [""])[0]
            if grant == "authorization_code":
                verifier = form.get("code_verifier", [""])[0]
                encoded = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
                valid = (
                    form.get("redirect_uri") == [self.server.redirect_uri]
                    and re.fullmatch(r"[A-Za-z0-9_.~-]{43,128}", verifier)
                    and self.server.codes.pop(form.get("code", [""])[0], None) == encoded
                )
                if valid:
                    self.server.token_grants.append("authorization_code")
                    self.server.access = FIRST_ACCESS
            elif grant == "refresh_token":
                valid = form.get("refresh_token") == [FIRST_REFRESH] and self.server.access == FIRST_ACCESS
                if valid:
                    self.server.token_grants.append("refresh_token")
                    self.server.access = SECOND_ACCESS
            else:
                valid = False
            access = self.server.access
        if not valid:
            self.json_response(400, {"error": "invalid_grant"})
            return
        self.json_response(200, {
            "access_token": access,
            "refresh_token": FIRST_REFRESH if access == FIRST_ACCESS else SECOND_REFRESH,
            "token_type": "Bearer", "expires_in": self.server.initial_lifetime if access == FIRST_ACCESS else 3600,
        })
        if access == SECOND_ACCESS:
            self.server.renewed.set()

    def upload(self, body):
        multipart = BytesParser(policy=policy.default).parsebytes(
            b"MIME-Version: 1.0\r\nContent-Type: "
            + self.headers.get("Content-Type", "").encode() + b"\r\n\r\n" + body
        )
        parts = {part.get_param("name", header="content-disposition"): part for part in multipart.iter_parts()}
        descriptions = {
            "purpose": b"vision", "expires_after[anchor]": b"created_at", "expires_after[seconds]": b"3600",
        }
        file = parts.get("file")
        if (not all(key in parts and parts[key].get_payload(decode=True) == value for key, value in descriptions.items())
                or file is None or file.get_content_type() != "image/png" or file.get_payload(decode=True) != PNG):
            self.json_response(400, {"error": "invalid image upload"})
            return
        with self.server.lock:
            self.server.uploads += 1
        self.json_response(201, {"id": FILE_ID})

    def quota(self):
        if not self.authorized():
            self.json_response(401, {"error": "unexpected quota credential"})
            return
        with self.server.lock:
            self.server.quota_reads += 1
        self.json_response(200, {
            "groups": [{"displayName": "Provider requests", "buckets": [{"window": "daily", "remainingFraction": 0.25}]}],
        })

    def generate(self, payload):
        with self.server.lock:
            self.server.generations.append((self.headers.get("Authorization"), payload))
        if payload.get("model") != UPSTREAM_MODEL:
            self.json_response(400, {"error": "model alias was not resolved"})
            return
        input_data = json.dumps(payload.get("input"))
        response = dict(RESPONSE_BODY)
        if "fixture-tool" in input_data or "fixture-undeclared" in input_data:
            tool = {
                "id": "fc_fixture", "call_id": "call_fixture", "type": "function_call", "arguments": '{"found":true}',
                "name": "undeclared_tool" if "fixture-undeclared" in input_data else DECLARED_TOOL, "status": "completed",
            }
            response["output"] = [*response["output"], tool]
        if not payload.get("stream"):
            self.json_response(200, response)
            return

        first = event_bytes({"type": "response.created", "response": {"id": "resp_fixture", "status": "in_progress"}})
        first += event_bytes({"type": "response.output_text.delta", "delta": MARKER})
        end = event_bytes({"type": "response.completed", "response": response})
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(first) + len(end)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(first)
        self.wfile.flush()
        if "fixture-tool" not in input_data or self.server.delta_received.wait(5):
            self.wfile.write(end)


@contextmanager
def fixture_server(redirect_uri, initial_lifetime):
    with http.server.ThreadingHTTPServer((LOOPBACK, 0), Fixture) as server:
        server.redirect_uri = redirect_uri
        server.initial_lifetime = initial_lifetime
        server.lock = threading.Lock()
        server.delta_received = threading.Event()
        server.renewed = threading.Event()
        server.codes = {}
        server.access = ""
        server.token_grants = []
        server.generations = []
        server.uploads = server.quota_reads = server.upstream_deletions = 0
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield server
        finally:
            server.delta_received.set()
            server.shutdown()
            thread.join(timeout=5)


def call(port, method, path, *, key=None, data=None, timeout=5):
    headers = {}
    if key is not None:
        headers["Authorization"] = "Bearer " + key
    if data is not None:
        headers["Content-Type"] = "application/json"
    with closing(http.client.HTTPConnection(LOOPBACK, port, timeout=timeout)) as client:
        client.request(method, path, None if data is None else json.dumps(data), headers)
        with client.getresponse() as reply:
            return reply.status, dict(reply.getheaders()), reply.read()


def checked_json(status, body, operation):
    require(status == 200, f"{operation} returned HTTP {status}: {body[:400]!r}")
    return json.loads(body)


def start_session(process, port, key):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline and process.poll() is None:
        try:
            status, _, body = call(port, "GET", MANAGEMENT + "/plugins", key=key, timeout=1)
            if status == 200 and b"cpa-bps" in body:
                status, _, body = call(port, "GET", MANAGEMENT + "/oauth/auth-url?provider=cpa-bps", key=key)
                return checked_json(status, body, "OAuth start")
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(0.2)
    raise RuntimeError("CPA plugin management routes did not become ready")


def login(process, port, issuer, key):
    login_session = start_session(process, port, key)
    authorize = urlsplit(login_session["url"])
    require(authorize.scheme == "http" and authorize.netloc == f"{LOOPBACK}:{issuer.server_port}"
            and authorize.path == "/authorize", "authorization URL did not point to the synthetic issuer")
    status, headers, _ = call(issuer.server_port, "GET", authorize.path + "?" + authorize.query)
    require(status == 302, "synthetic issuer rejected the authorization parameters")
    callback = headers["Location"]
    callback_url = urlsplit(callback)
    require(callback.split("?")[0] == issuer.redirect_uri and parse_qs(callback_url.query).get("state") == [login_session["state"]],
            "callback did not retain the configured redirect URI and one-time state")
    status, _, body = call(port, "GET", callback_url.path + "?" + callback_url.query)
    checked_json(status, body, "OAuth callback")

    deadline = time.monotonic() + 10
    state_path = MANAGEMENT + "/oauth/status?" + urlencode({"state": login_session["state"]})
    phase = None
    while time.monotonic() < deadline:
        status, _, body = call(port, "GET", state_path, key=key)
        result = checked_json(status, body, "OAuth status")
        phase = result.get("status")
        if phase == "ok":
            break
        require(phase == "wait", f"OAuth authorization failed: {result}")
        time.sleep(0.1)
    require(phase == "ok", "OAuth authorization did not finish")
    status, _, body = call(port, "GET", MANAGEMENT + "/credentials", key=key)
    records = checked_json(status, body, "credential listing")["files"]
    require(len(records) == 1 and records[0].get("auth_index") and records[0].get("name"),
            "CPA did not publish exactly one credential with a name and index")
    return records[0]


def assert_quota(port, key, auth_index, expected_completed):
    status, _, body = call(port, "POST", MANAGEMENT + "/plugins/cpa-bps/quota", key=key,
                           data={"auth_index": auth_index})
    result = checked_json(status, body, "quota fetch")
    require(result["groups"][0]["buckets"][0]["remainingFraction"] == 0.25,
            "quota result did not retain the provider's quota fraction")
    metrics = {metric["key"]: metric["value"] for metric in result["summary"]}
    require(metrics.get("local_completed") == expected_completed,
            f"unexpected local completed-request count: {metrics.get('local_completed')}")


def request_after_registration(port, issuer, payload, observe_stream=None):
    # CPA publishes the model and scheduler entries separately after a credential change.
    # Retry only a fixture-local rejection that reached neither upload nor generation.
    deadline = time.monotonic() + 5
    with issuer.lock:
        before = (issuer.uploads, len(issuer.generations))
    while True:
        result = request(port, "/v1/responses", payload, observe_stream)
        status, _, body = result
        if status != 503 or b"auth_not_found" not in body or time.monotonic() >= deadline:
            return result
        with issuer.lock:
            require(before == (issuer.uploads, len(issuer.generations)),
                    "a registration rejection unexpectedly reached the fixture")
        time.sleep(0.1)


def smoke(process, port, issuer, key):
    credential = login(process, port, issuer, key)
    start_ready(process, port, MODEL)
    status, headers, body = call(port, "GET", "/v0/resource/plugins/cpa-bps/index.html")
    require(status == 200 and "text/html" in headers.get("Content-Type", "") and
            b"BPS" in body and b'data-oauth-ready="true"' in body,
            "Manager authorization page was unavailable")

    status, kind, body = request_after_registration(port, issuer, {"model": MODEL, "input": [{"role": "user", "content": [
        {"type": "input_text", "text": "fixture-image"},
        {"type": "input_image", "image_url": "data:image/png;base64," + PNG_BASE64, "detail": "low"},
    ]}]})
    response = checked_json(status, body, "nonstream image request")
    require("application/json" in kind and response["output"][0]["content"][0]["text"] == MARKER,
            "nonstream image result did not contain the fixture text")
    with issuer.lock:
        first_token, payload = issuer.generations[0]
        part = payload["input"][0]["content"][1]
        require(issuer.uploads == 1 and first_token == "Bearer " + FIRST_ACCESS and
                part.get("file_id") == FILE_ID and part.get("detail") == "low" and "image_url" not in part,
                "OAuth delivery or image conversion failed before generation")

    auth_index = credential["auth_index"]
    with issuer.lock:
        admission_quota_reads = issuer.quota_reads
    assert_quota(port, key, auth_index, expected_completed=1)
    assert_quota(port, key, auth_index, expected_completed=1)
    status, _, body = call(port, "DELETE", MANAGEMENT + "/plugins/cpa-bps/quota?" + urlencode(
        {"auth_index": auth_index}), key=key)
    require(checked_json(status, body, "local quota reset").get("status") == "ok", "local quota reset did not complete")
    assert_quota(port, key, auth_index, expected_completed=0)
    with issuer.lock:
        require(issuer.quota_reads == admission_quota_reads + 2 and issuer.upstream_deletions == 0,
                "local quota reset did not invalidate only the local cache")

    status, _, body = call(port, "POST", MANAGEMENT + "/credentials/refresh", key=key,
                           data={"name": credential["name"]})
    require(checked_json(status, body, "OAuth refresh").get("ok") is True, "OAuth refresh did not complete")

    def observe_delta(event):
        if event.get("type") == "response.output_text.delta" and event.get("delta") == MARKER:
            issuer.delta_received.set()

    status, kind, events = request_after_registration(port, issuer, {
        "model": MODEL, "input": "fixture-tool", "stream": True, "tools": [TOOL],
    }, observe_delta)
    require(status == 200 and "text/event-stream" in kind and issuer.delta_received.is_set(),
            "text delta did not arrive before stream completion")
    completed = [event for event in events if event.get("type") == "response.completed"]
    require(len(completed) == 1 and any(item.get("name") == DECLARED_TOOL for item in completed[0]["response"]["output"]),
            "declared tool did not pass through the terminal event")
    status, _, body = request(port, "/v1/responses", {"model": MODEL, "input": "fixture-undeclared", "tools": [TOOL]})
    require(status == 502 and b"bps_invalid_tool_response" in body, "undeclared upstream tool call was not rejected")
    with issuer.lock:
        require(issuer.token_grants == ["authorization_code", "refresh_token"], "expected one login and one refresh grant")
        require([token for token, _ in issuer.generations] ==
                ["Bearer " + FIRST_ACCESS, "Bearer " + SECOND_ACCESS, "Bearer " + SECOND_ACCESS],
                "generation did not switch to the refreshed access token")
    print("PASS isolated CPA: OAuth+PKCE, refresh, Manager page, Models, Files, quota cache/reset, Responses and guarded SSE")


def smoke_auto_refresh(process, port, issuer, key, auths):
    credential = login(process, port, issuer, key)
    require(issuer.renewed.wait(12), "CPA did not schedule an OAuth refresh for the short-lived grant")
    name = pathlib.Path(credential["name"]).name
    require(name == credential["name"], "the fixture credential name is not local to the auth directory")
    credential_path = auths / name
    deadline = time.monotonic() + 5
    persisted = False
    while time.monotonic() < deadline:
        try:
            grant = json.loads(credential_path.read_text()).get("oauth_grant", {})
            persisted = grant.get("access_token") == SECOND_ACCESS and grant.get("refresh_token") == SECOND_REFRESH
        except (OSError, ValueError, AttributeError):
            pass
        if persisted:
            break
        time.sleep(0.1)
    require(persisted, "CPA did not persist both rotated OAuth tokens")
    start_ready(process, port, MODEL)
    status, _, body = request_after_registration(port, issuer, {"model": MODEL, "input": "fixture-auto-refresh"})
    require(checked_json(status, body, "request after automatic refresh").get("status") == "completed",
            "generation failed after automatic OAuth refresh")
    with issuer.lock:
        require(issuer.token_grants == ["authorization_code", "refresh_token"] and
                [token for token, _ in issuer.generations] == ["Bearer " + SECOND_ACCESS],
                "scheduled renewal did not rotate the test grant once")
    print("PASS isolated CPA: scheduled OAuth refresh persisted the rotated grant and used its new access token")


def run(cpa, plugin, auto_refresh=False):
    with tempfile.TemporaryDirectory(prefix="cpa-bps-full-") as workdir:
        root = pathlib.Path(workdir)
        auths, plugins = root / "auths", root / "plugins"
        auths.mkdir(mode=0o700)
        plugins.mkdir(mode=0o700)
        shutil.copy2(plugin, plugins / ("cpa-bps" + plugin.suffix))
        port = reserve_port()
        key = secrets.token_urlsafe(24)
        lifetime = 4 if auto_refresh else 3600
        with fixture_server(f"http://{LOOPBACK}:{port}{CALLBACK}", lifetime) as issuer:
            origin = f"http://{LOOPBACK}:{issuer.server_port}"
            cfg = root / "config.yaml"
            cfg.write_text(json.dumps({
                "host": LOOPBACK, "port": port, "auth-dir": str(auths), "api-keys": [API_KEY], "request-retry": 0,
                "management": {"secret-key": key, "disable-control-panel": True, "disable-auto-update-panel": True},
                "plugins": {"enabled": True, "dir": str(plugins), "configs": {"cpa-bps": {
                    "enabled": True, "responses_url": origin + "/v1/responses", "files_url": origin + "/v1/files",
                    "quota_url": origin + "/v1/quota", "models": [{"name": UPSTREAM_MODEL, "alias": MODEL}],
                    "oauth": {"authorization_url": origin + "/authorize", "token_url": origin + "/token",
                              "client_id": CLIENT_ID, "redirect_uri": issuer.redirect_uri, "scopes": ["responses"]},
                }}},
            }))
            env = {"XDG_CONFIG_HOME": workdir, "XDG_CACHE_HOME": workdir, "TMPDIR": workdir,
                   "HTTP_PROXY": origin, "HTTPS_PROXY": origin, "NO_PROXY": LOOPBACK}
            log_path = root / "cpa.log"
            with log_path.open("wb") as log:
                process = subprocess.Popen([str(cpa), "-local-model", "-config", str(cfg)],
                                           cwd=root, env=env, stdout=log, stderr=log)
                try:
                    if auto_refresh:
                        smoke_auto_refresh(process, port, issuer, key, auths)
                    else:
                        smoke(process, port, issuer, key)
                except Exception as error:
                    log.flush()
                    raise RuntimeError(f"{error}\nCPA diagnostic log:\n{read_diagnostics(log_path)}") from error
                finally:
                    issuer.delta_received.set()
                    if process.poll() is None:
                        process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", required=True, type=pathlib.Path, help="CPA executable")
    parser.add_argument("--plugin", required=True, type=pathlib.Path, help="native plugin library")
    parser.add_argument("--auto-refresh", action="store_true", help="also test scheduled refresh in a separate CPA")
    args = parser.parse_args()
    cpa, plugin = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    run(cpa, plugin)
    if args.auto_refresh:
        run(cpa, plugin, auto_refresh=True)


if __name__ == "__main__":
    main()
