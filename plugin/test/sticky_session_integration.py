#!/usr/bin/env python3
"""Exercise session affinity using an isolated CPA and two fake accounts.

No existing auth directory, real BPS endpoint, or running CPA service is used.
Both prompt_cache_key and X-Session-ID are tested in separate temporary processes.
"""

import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from datetime import datetime, timezone
import hashlib
import http.client
import http.server
import json
import pathlib
import re
import secrets
import shutil
import signal
import subprocess
import tempfile
import threading
import time

from integration import LOOPBACK, event_bytes, read_diagnostics, require, reserve_port


MODEL = "fixture-sticky-model"
UPSTREAM_MODEL = "fixture-sticky-upstream"
API_KEY = "fixture-sticky-client"
TOKENS = {"account_A": "fixture-sticky-token-A", "account_B": "fixture-sticky-token-B"}
TOKEN_ENVS = {label: "CPA_BPS_STICKY_" + label[-1] for label in TOKENS}
REQUEST_PATTERN = re.compile(r"FIXTURE_REQ_([a-z0-9_]+)")
MAX_BODY = 1 << 20
CONCURRENT_REQUESTS = 4


class Fixture(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def reply(self, status, body, content_type="application/json"):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_CONNECT(self):
        # Deny background update traffic through the isolated process's proxy.
        self.reply(403, b"{}")

    def do_GET(self):
        self.reply(404, b"{}")

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        if self.path != "/v1/responses" or not 0 < length <= MAX_BODY:
            self.reply(400, b"{}")
            return
        payload = json.loads(self.rfile.read(length))
        account = next((label for label, token in TOKENS.items()
                        if self.headers.get("Authorization") == "Bearer " + token), None)
        markers = REQUEST_PATTERN.findall(json.dumps(payload.get("input")))
        if account is None or not markers or payload.get("model") != UPSTREAM_MODEL:
            self.reply(400, b'{"error":{"code":"invalid_fixture_request"}}')
            return
        request_id = markers[-1]
        gated = request_id.startswith("parallel_")
        with self.server.lock:
            self.server.requests.append({"request": request_id, "account": account})
            if gated:
                self.server.active += 1
                self.server.peak_active = max(self.server.peak_active, self.server.active)
                if self.server.active == CONCURRENT_REQUESTS:
                    self.server.parallel_entered.set()
        try:
            if gated and not self.server.release_parallel.wait(10):
                self.reply(504, b'{"error":{"code":"fixture_gate_timeout"}}')
                return
            if request_id == "transient_failure" and account == self.server.fail_account:
                self.reply(503, b'{"error":{"code":"fixture_unavailable","message":"synthetic 503"}}')
                return
            text = "ACK " + request_id
            response = {
                "id": "resp_" + request_id, "object": "response", "status": "completed",
                "model": UPSTREAM_MODEL,
                "output": [{"id": "msg_" + request_id, "type": "message", "role": "assistant",
                            "status": "completed", "content": [
                                {"type": "output_text", "text": text, "annotations": []}]}],
                "usage": {"input_tokens": 5, "output_tokens": 2, "total_tokens": 7},
            }
            if payload.get("stream"):
                body = event_bytes({"type": "response.created", "response": {
                    "id": response["id"], "status": "in_progress"}})
                body += event_bytes({"type": "response.output_text.delta", "delta": text})
                if request_id != "partial_stream":
                    body += event_bytes({"type": "response.completed", "response": response})
                self.reply(200, body, "text/event-stream")
            else:
                self.reply(200, json.dumps(response).encode())
        finally:
            if gated:
                with self.server.lock:
                    self.server.active -= 1


def call(port, method, path, *, data=None, key=API_KEY, session_header=None, timeout=15):
    headers = {"Authorization": "Bearer " + key}
    body = None
    if data is not None:
        headers["Content-Type"] = "application/json"
        body = json.dumps(data).encode()
    if session_header is not None:
        headers["X-Session-ID"] = session_header
    with closing(http.client.HTTPConnection(LOOPBACK, port, timeout=timeout)) as conn:
        conn.request(method, path, body, headers)
        with conn.getresponse() as reply:
            return reply.status, reply.getheader("Content-Type", ""), reply.read(MAX_BODY + 1)


def credentials(port, key):
    status, _, body = call(port, "GET", "/v8/management/credentials", key=key)
    require(status == 200, "isolated CPA credential listing failed")
    files = json.loads(body).get("files", [])
    require(len(files) == 2, "isolated CPA must contain exactly two synthetic credentials")
    return {label: next(row for row in files if row["name"] == label + ".json") for label in TOKENS}


def set_disabled(port, key, account, disabled):
    status, _, _ = call(port, "PATCH", "/v8/management/credentials/status", key=key,
                        data={"name": account["name"], "auth_index": account["auth_index"], "disabled": disabled})
    require(status == 200, "synthetic credential status mutation failed")
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        current = credentials(port, key)
        if any(row["auth_index"] == account["auth_index"] and bool(row.get("disabled")) == disabled
               for row in current.values()):
            time.sleep(.3)
            return
        time.sleep(.05)
    raise RuntimeError("synthetic credential status did not converge")


class Suite:
    def __init__(self, port, upstream, management_key, mode, rebind_on):
        self.port, self.upstream, self.key, self.mode = port, upstream, management_key, mode
        self.rebind_on = rebind_on
        self.reports = []
        self.session = "fixture-session-" + secrets.token_hex(8)
        self.history = []
        self.owner = None

    def observed(self, request_id):
        with self.upstream.lock:
            return [row["account"] for row in self.upstream.requests if row["request"] == request_id]

    def generate(self, request_id, *, session=None, history=None, stream=False, allow_partial=False):
        messages = list(history or []) + [{"role": "user", "content": "FIXTURE_REQ_" + request_id}]
        session = self.session if session is None else session
        body = {"model": MODEL, "input": messages, "stream": stream, "store": False}
        if self.mode == "prompt_cache_key":
            body["prompt_cache_key"] = session
        status, kind, raw = call(self.port, "POST", "/v1/responses", data=body,
                                 session_header=session if self.mode == "header" else None)
        require(len(raw) <= MAX_BODY, "isolated CPA response exceeded test limit")
        response = None
        stream_failure = False
        if status == 200 and "text/event-stream" in kind:
            events = [json.loads(line[5:].strip()) for line in raw.splitlines()
                      if line.startswith(b"data:") and line[5:].strip() not in (b"", b"[DONE]")]
            terminals = [event["response"] for event in events if event.get("type") == "response.completed"]
            if allow_partial:
                require(not terminals, "incomplete SSE fabricated a completed response")
                require(events and events[-1].get("type") in {"error", "response.error", "response.failed"},
                        "incomplete SSE did not expose a terminal failure")
                stream_failure = True
            else:
                require(len(terminals) == 1, "SSE must have exactly one completed response")
                response = terminals[0]
            text = "".join(event.get("delta", "") for event in events if event.get("type") == "response.output_text.delta")
            require(text == "ACK " + request_id, "SSE output mismatch")
        elif "application/json" in kind:
            response = json.loads(raw)
        accounts = self.observed(request_id)
        error = response.get("error", {}) if isinstance(response, dict) else {}
        affinity_error = None
        if isinstance(error, dict):
            # Responses wraps local affinity errors in protocol-level error codes.
            for code in ("session_auth_unavailable", "session_auth_retry"):
                if error.get("code") == code or str(error.get("message", "")).startswith(code + ":"):
                    affinity_error = code
        report = {"request": request_id, "http": status, "upstream_accounts": accounts,
                  "error_code": error.get("code") if isinstance(error, dict) else None,
                  "affinity_error": affinity_error, "stream_failed_after_delta": stream_failure}
        self.reports.append(report)
        if status == 200 and not stream_failure:
            require(response and response.get("status") == "completed", "successful request was not completed")
            text = "".join(part.get("text", "") for item in response["output"]
                           for part in item.get("content", []) if part.get("type") == "output_text")
            require(text == "ACK " + request_id, "JSON output mismatch")
            expected_attempts = 2 if self.rebind_on == "any" and request_id == "transient_failure" else 1
            require(len(accounts) == expected_attempts, "unexpected upstream attempt count for a successful request")
        return report, messages + (response.get("output", []) if response and status == 200 else [])

    def success(self, request_id, **kwargs):
        report, history = self.generate(request_id, **kwargs)
        require(report["http"] == 200, f"{request_id}: expected 200, observed {report}")
        return report["upstream_accounts"][0], history

    def run(self):
        for turn in range(6):
            owner, self.history = self.success("turn_" + str(turn), history=self.history, stream=bool(turn % 2))
            if self.owner is None:
                self.owner = owner
            require(owner == self.owner, "six-turn session changed accounts")
        new_owners = {self.owner}
        for index in range(3):
            owner, _ = self.success("fresh_" + str(index), session="fixture-fresh-" + secrets.token_hex(8))
            new_owners.add(owner)
        require(new_owners == set(TOKENS), "fresh sessions did not distribute across both accounts")

        # All four requests must be in the real upstream simultaneously.
        with ThreadPoolExecutor(max_workers=CONCURRENT_REQUESTS) as pool:
            futures = [pool.submit(self.success, "parallel_" + str(index), history=self.history, stream=True)
                       for index in range(CONCURRENT_REQUESTS)]
            try:
                entered = self.upstream.parallel_entered.wait(10)
            finally:
                self.upstream.release_parallel.set()
            results = [future.result(timeout=20) for future in futures]
        require(entered and self.upstream.peak_active == CONCURRENT_REQUESTS,
                "same-session requests were not simultaneously active upstream")
        require(all(owner == self.owner for owner, _ in results), "concurrent same-session requests changed accounts")

        self.upstream.fail_account = self.owner
        if self.rebind_on == "any":
            return self.automatic_failover()

        accounts = credentials(self.port, self.key)
        set_disabled(self.port, self.key, accounts[self.owner], True)
        try:
            blocked, _ = self.generate("disabled_binding", history=self.history)
            require(blocked["http"] == 429 and blocked["affinity_error"] == "session_auth_unavailable",
                    f"disabled binding did not produce the explicit strict-affinity error: {blocked}")
            require(not blocked["upstream_accounts"], "disabled session silently switched accounts")
            other, _ = self.success("healthy_while_disabled", session="fixture-healthy-" + secrets.token_hex(8))
            require(other != self.owner, "new session did not use the remaining healthy credential")
        finally:
            set_disabled(self.port, self.key, accounts[self.owner], False)
        recovered, _ = self.success("reenabled_binding", history=self.history)
        require(recovered == self.owner, "reenabling the bound account lost its session binding")

        failed, _ = self.generate("transient_failure", history=self.history)
        require(failed["http"] == 503 and failed["upstream_accounts"] == [self.owner],
                f"upstream 503 switched accounts or was replayed: {failed}")
        cooled, _ = self.generate("cooled_binding", history=self.history)
        require(cooled["http"] == 503 and cooled["affinity_error"] == "session_auth_retry",
                f"cooled binding did not return an explicit retry error: {cooled}")
        require(not cooled["upstream_accounts"], "cooled session contacted another account")
        healthy, _ = self.success("healthy_after_503", session="fixture-healthy-" + secrets.token_hex(8))
        require(healthy != self.owner, "new session failed to use the other account after 503")

        with self.upstream.lock:
            counts = Counter(row["request"] for row in self.upstream.requests)
        require(all(count == 1 for count in counts.values()), "automatic upstream replay detected")
        return {
            "passed": True, "session_field": self.mode, "rebind_on": self.rebind_on, "six_turn_owner": self.owner,
            "fresh_sessions_use_both_accounts": True, "concurrent_same_session": CONCURRENT_REQUESTS,
            "strict_disabled_binding": True, "strict_503_binding": True,
            "successful_requests_replayed": 0, "upstream_generations": sum(counts.values()),
            "requests": self.reports,
        }

    def automatic_failover(self):
        other = next(account for account in TOKENS if account != self.owner)
        failed_over, _ = self.generate("transient_failure", history=self.history)
        require(failed_over["http"] == 200 and failed_over["upstream_accounts"] == [self.owner, other],
                f"one upstream 503 did not fail over once to the other account: {failed_over}")
        # Any-mode uses a one-second synthetic cooldown. Prove the original account
        # is selectable again before checking that the recovered account does not win back S.
        time.sleep(1.2)
        recovered = False
        for index in range(3):
            selected, _ = self.success("recovery_probe_" + str(index),
                                       session="fixture-recovery-" + secrets.token_hex(8))
            if selected == self.owner:
                recovered = True
                break
        require(recovered, "original account did not become available after synthetic cooldown")
        retained, _ = self.success("keep_rebound_after_recovery", history=self.history)
        require(retained == other, "session jumped back when the original account recovered")

        accounts = credentials(self.port, self.key)
        set_disabled(self.port, self.key, accounts[other], True)
        try:
            switched, _ = self.success("disabled_binding_switch", history=self.history)
            require(switched == self.owner, "disabled binding did not switch to the available account")
        finally:
            set_disabled(self.port, self.key, accounts[other], False)
        retained, _ = self.success("keep_binding_after_reenable", history=self.history)
        require(retained == self.owner, "reenabling the old binding moved the active session")

        partial, _ = self.generate("partial_stream", history=self.history, stream=True, allow_partial=True)
        require(partial["http"] == 200 and partial["stream_failed_after_delta"]
                and partial["upstream_accounts"] == [self.owner],
                "a stream that already delivered text was replayed or lost its terminal error")
        with self.upstream.lock:
            counts = Counter(row["request"] for row in self.upstream.requests)
        require(counts["transient_failure"] == 2
                and all(count == 1 for request_id, count in counts.items() if request_id != "transient_failure"),
                "successful generation was replayed or failover exceeded two upstream attempts")
        return {
            "passed": True, "session_field": self.mode, "rebind_on": self.rebind_on,
            "six_turn_owner": self.owner, "fresh_sessions_use_both_accounts": True,
            "concurrent_same_session": CONCURRENT_REQUESTS, "503_failover_attempts": 2,
            "rebound_preserved_after_recovery": True, "disabled_binding_switches": True,
            "successful_requests_replayed": 0, "partial_stream_replayed": False,
            "upstream_generations": sum(counts.values()), "requests": self.reports,
        }


def ready(process, port):
    deadline = time.monotonic() + 20
    while process.poll() is None and time.monotonic() < deadline:
        try:
            status, _, raw = call(port, "GET", "/v1/models", timeout=1)
            if status == 200 and MODEL in {item["id"] for item in json.loads(raw).get("data", [])}:
                return
        except (OSError, ValueError, http.client.HTTPException):
            pass
        time.sleep(.1)
    raise RuntimeError("isolated CPA did not become ready")


def run(cpa, plugin, mode, rebind_on):
    with tempfile.TemporaryDirectory(prefix="cpa-bps-sticky-") as workdir:
        root = pathlib.Path(workdir)
        auths, plugins = root / "auths", root / "plugins"
        auths.mkdir(mode=0o700)
        plugins.mkdir(mode=0o700)
        shutil.copy2(plugin, plugins / ("cpa-bps" + plugin.suffix))
        with http.server.ThreadingHTTPServer((LOOPBACK, 0), Fixture) as upstream:
            upstream.lock = threading.Lock()
            upstream.requests = []
            upstream.active = upstream.peak_active = 0
            upstream.fail_account = None
            upstream.parallel_entered = threading.Event()
            upstream.release_parallel = threading.Event()
            thread = threading.Thread(target=upstream.serve_forever, daemon=True)
            thread.start()
            origin = f"http://{LOOPBACK}:{upstream.server_port}"
            for label, env_name in TOKEN_ENVS.items():
                (auths / (label + ".json")).write_text(json.dumps({
                    "type": "cpa-bps", "label": label, "token_env": env_name, "upstream_origin": origin,
                }))
            port, management_key = reserve_port(), secrets.token_hex(24)
            config = {
                "host": LOOPBACK, "port": port, "auth-dir": str(auths), "api-keys": [API_KEY],
                "management": {"secret-key": management_key, "disable-control-panel": True,
                               "disable-auto-update-panel": True},
                "routing": {"strategy": "round-robin", "session-affinity": True,
                            "session-affinity-ttl": "24h", "session-affinity-rebind-on": rebind_on,
                            "retry": {"request-retry": 0, "max-retry-credentials": 2, "max-retry-interval": 0},
                            "cooldown": {"transient-error-cooldown-seconds": 1 if rebind_on == "any" else 60}},
                "request-retry": 0, "max-retry-credentials": 2, "max-retry-interval": 0,
                "plugins": {"enabled": True, "dir": str(plugins), "configs": {"cpa-bps": {
                    "enabled": True, "responses_url": origin + "/v1/responses",
                    "models": [{"name": UPSTREAM_MODEL, "alias": MODEL}],
                    "account_concurrency_limit": CONCURRENT_REQUESTS,
                }}},
            }
            config_path = root / "config.yaml"
            config_path.write_text(json.dumps(config))
            env = {"XDG_CONFIG_HOME": workdir, "XDG_CACHE_HOME": workdir, "TMPDIR": workdir,
                   "HTTP_PROXY": origin, "HTTPS_PROXY": origin, "NO_PROXY": LOOPBACK,
                   **{TOKEN_ENVS[label]: token for label, token in TOKENS.items()}}
            log_path = root / "cpa.log"
            process = None
            result = None
            try:
                with log_path.open("wb") as log:
                    process = subprocess.Popen([str(cpa), "-local-model", "-config", str(config_path)],
                                               cwd=root, env=env, stdout=log, stderr=log)
                    suite = Suite(port, upstream, management_key, mode, rebind_on)
                    try:
                        ready(process, port)
                        result = suite.run()
                        print(json.dumps(result), flush=True)
                        return result
                    except Exception as error:
                        log.flush()
                        raise RuntimeError(
                            f"{mode}: {error}\nObserved requests: {json.dumps(suite.reports)}\n"
                            f"CPA diagnostic log:\n{read_diagnostics(log_path)}"
                        ) from error
                    finally:
                        upstream.release_parallel.set()
                        if process.poll() is None:
                            process.terminate()
                        try:
                            process.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait(timeout=5)
                        if result is not None:
                            result["isolated_process_cleaned_up"] = process.poll() is not None
            finally:
                upstream.release_parallel.set()
                upstream.shutdown()
                thread.join(timeout=5)


def interrupted(*_):
    raise KeyboardInterrupt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=pathlib.Path, required=True)
    parser.add_argument("--plugin", type=pathlib.Path, required=True)
    parser.add_argument("--rebind-on", choices=("any", "never"), default="any")
    parser.add_argument("--evidence", type=pathlib.Path, help="write a new JSON evidence file after both modes finish")
    args = parser.parse_args()
    cpa, plugin = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    if args.evidence is not None and args.evidence.exists():
        parser.error("the evidence file already exists; choose a new path")
    evidence = {
        "started_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "passed": False, "synthetic_loopback_only": True, "rebind_on": args.rebind_on,
        "session_affinity_ttl": "24h", "request_retry": 0, "max_retry_credentials": 2,
        "plugin_sha256": hashlib.sha256(plugin.read_bytes()).hexdigest(),
        "cpa_sha256": hashlib.sha256(cpa.read_bytes()).hexdigest(), "runs": [],
    }
    previous = signal.signal(signal.SIGTERM, interrupted)
    try:
        for mode in ("prompt_cache_key", "header"):
            evidence["runs"].append(run(cpa, plugin, mode, args.rebind_on))
        evidence["passed"] = all(result["passed"] and result["isolated_process_cleaned_up"]
                                 for result in evidence["runs"])
    finally:
        signal.signal(signal.SIGTERM, previous)
        evidence["finished_at"] = datetime.now(timezone.utc).isoformat(timespec="seconds")
        if args.evidence is not None:
            args.evidence.parent.mkdir(parents=True, exist_ok=True)
            with args.evidence.open("x", encoding="utf-8") as file:
                json.dump(evidence, file, ensure_ascii=False, indent=2)
                file.write("\n")


if __name__ == "__main__":
    main()

