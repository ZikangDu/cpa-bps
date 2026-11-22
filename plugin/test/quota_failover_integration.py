#!/usr/bin/env python3
"""Verify quota-aware failover with an isolated CPA and two loopback accounts.

No existing credential directory, production service, or real upstream is used.
The fixture counts every generation and quota GET; HTTP success alone is not
sufficient to pass. The same session must remain on its replacement account.
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
import math
import pathlib
import re
import secrets
import shutil
import signal
import subprocess
import tempfile
import threading
import time

from integration import LOOPBACK, read_diagnostics, require, reserve_port


API_KEY = "fixture-quota-client"
MODELS = {"fixture-quota-one": "fixture-upstream-one", "fixture-quota-two": "fixture-upstream-two"}
TOKENS = {"account_A": "fixture-quota-token-A", "account_B": "fixture-quota-token-B"}
TOKEN_ENVS = {label: "CPA_BPS_QUOTA_" + label[-1] for label in TOKENS}
MARKER = re.compile(r"FIXTURE_QUOTA_([a-z0-9_]+)")
MAX_BODY = 1 << 20


def utc_time(epoch):
    return datetime.fromtimestamp(epoch, timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


class ConcurrentFixtureServer(http.server.ThreadingHTTPServer):
    # Account failover opens a burst of fresh connections. Keep the synthetic
    # server accept backlog above the burst so kernel resets cannot masquerade
    # as plugin transport failures.
    request_queue_size = 128


class Fixture(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def reply(self, status, body, headers=None):
        encoded = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Connection", "close")
        for name, value in (headers or {}).items():
            self.send_header(name, str(value))
        self.end_headers()
        try:
            self.wfile.write(encoded)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def account(self):
        return next((label for label, token in self.server.tokens.items()
                     if self.headers.get("Authorization") == "Bearer " + token), None)

    def do_CONNECT(self):
        # Deny CPA background update traffic through the isolated proxy.
        self.reply(403, {})

    def do_GET(self):
        account = self.account()
        if self.path != "/v1/quota" or account is None:
            self.reply(404, {})
            return
        with self.server.lock:
            self.server.quota_reads.append(account)
            state = dict(self.server.accounts[account])
        remaining = state["remaining"]
        if state["auto_restore"] and time.time() >= state["reset"]:
            remaining = 1.0
        self.reply(200, {"groups": [{"displayName": "Fixture account quota", "buckets": [{
            "window": "7d", "remainingFraction": remaining,
            "resetTime": utc_time(state["reset"] if remaining == 0 else time.time() + 3600),
        }]}]})

    def do_POST(self):
        account = self.account()
        length = int(self.headers.get("Content-Length", "0"))
        if self.path != "/v1/responses" or account is None or not 0 < length <= MAX_BODY:
            self.reply(400, {"error": {"code": "invalid_fixture_request"}})
            return
        payload = json.loads(self.rfile.read(length))
        markers = MARKER.findall(json.dumps(payload.get("input")))
        if not markers or payload.get("model") not in MODELS.values():
            self.reply(400, {"error": {"code": "invalid_fixture_payload"}})
            return
        request_id = markers[-1]
        with self.server.lock:
            self.server.generations.append({"request": request_id, "account": account,
                                            "model": payload["model"]})
            state = dict(self.server.accounts[account])
        available = state["remaining"] > 0 or (state["auto_restore"] and time.time() >= state["reset"])
        if (request_id.startswith("parallel_switch_")
                and account == getattr(self.server, "reject_account", None)):
            # Force all twelve requests past the cached positive admission
            # before the first 429 teaches CPA that this account is exhausted.
            self.server.reject_barrier.wait(timeout=8)
        if not available:
            # Intentionally generic and short: the quota GET must establish the
            # actual account-wide recovery time rather than trusting this delay.
            self.reply(429, {"error": {"code": "rate_limit_exceeded", "type": "rate_limit_error"}},
                       {"Retry-After": "1"})
            return
        self.reply(200, {
            "id": "resp_" + request_id, "object": "response", "status": "completed",
            "model": payload["model"],
            "output": [{"id": "msg_" + request_id, "type": "message", "role": "assistant",
                        "status": "completed", "content": [
                            {"type": "output_text", "text": "ACK " + request_id, "annotations": []}]}],
            "usage": {"input_tokens": 5, "output_tokens": 2, "total_tokens": 7},
        })


def call(port, request_id, *, model=None, session=None, timeout=12):
    payload = {"model": model or next(iter(MODELS)),
               "input": "FIXTURE_QUOTA_" + request_id, "stream": False,
               "store": False, "prompt_cache_key": session or "fixture-session-" + request_id}
    started = time.monotonic()
    with closing(http.client.HTTPConnection(LOOPBACK, port, timeout=timeout)) as conn:
        conn.request("POST", "/v1/responses", json.dumps(payload), {
            "Authorization": "Bearer " + API_KEY, "Content-Type": "application/json"})
        with conn.getresponse() as reply:
            raw = reply.read(MAX_BODY + 1)
            require(len(raw) <= MAX_BODY, "response exceeded fixture bound")
            value = json.loads(raw)
            error = value.get("error", {})
            return {"request": request_id, "http": reply.status,
                    "retry_after": reply.getheader("Retry-After"),
                    "duration_s": round(time.monotonic() - started, 4),
                    "error_code": error.get("code") if isinstance(error, dict) else None,
                    "body": value}


def ready(process, port):
    deadline = time.monotonic() + 20
    while process.poll() is None and time.monotonic() < deadline:
        try:
            with closing(http.client.HTTPConnection(LOOPBACK, port, timeout=1)) as conn:
                conn.request("GET", "/v1/models", headers={"Authorization": "Bearer " + API_KEY})
                with conn.getresponse() as reply:
                    value = json.loads(reply.read())
                    if reply.status == 200 and set(MODELS).issubset({m["id"] for m in value.get("data", [])}):
                        return
        except (OSError, ValueError, http.client.HTTPException):
            pass
        threading.Event().wait(.1)
    raise RuntimeError("isolated CPA did not expose both fixture models")


class Suite:
    def __init__(self, port, fixture):
        self.port, self.fixture = port, fixture
        self.reports = []

    def snapshots(self):
        with self.fixture.lock:
            return list(self.fixture.generations), list(self.fixture.quota_reads)

    def set_quota(self, accounts, remaining, reset, auto_restore=False):
        with self.fixture.lock:
            for account in accounts:
                self.fixture.accounts[account] = {"remaining": remaining, "reset": reset,
                                                   "auto_restore": auto_restore}

    def request(self, request_id, **kwargs):
        result = call(self.port, request_id, **kwargs)
        generations, _ = self.snapshots()
        result["upstream_accounts"] = [g["account"] for g in generations if g["request"] == request_id]
        body = result.pop("body")
        if result["http"] == 200:
            require(body.get("status") == "completed", "successful response is not completed")
            text = "".join(c.get("text", "") for item in body.get("output", [])
                           for c in item.get("content", []) if c.get("type") == "output_text")
            require(text == "ACK " + request_id, "successful response body mismatch")
        self.reports.append(result)
        return result

    def exhausted(self, result, reset, *, min_remaining=1):
        require(result["http"] == 429, f"expected quota HTTP 429: {result}")
        require(result["duration_s"] < 3, f"exhausted request waited/retried internally: {result}")
        try:
            retry = int(result["retry_after"])
        except (TypeError, ValueError):
            raise RuntimeError(f"quota response lacks numeric Retry-After: {result}") from None
        remaining = max(min_remaining, math.ceil(reset - time.time()))
        require(max(min_remaining, remaining - 1) <= retry <= remaining + 3,
                f"Retry-After does not reflect quota reset (remaining={remaining}): {result}")

    def run(self):
        # Set the short window after readiness so startup cannot consume it.
        reset = time.time() + 12
        self.set_quota(TOKENS, 0, reset, auto_restore=True)
        initial = self.request("initial_empty")
        self.exhausted(initial, reset)
        require(not initial["upstream_accounts"], "fresh zero quota still sent a generation")
        _, quota_reads = self.snapshots()
        require(set(quota_reads) == set(TOKENS), "both candidate accounts were not inspected")
        before_generations, before_reads = self.snapshots()
        for index in range(4):
            result = self.request("empty_repeat_" + str(index), model=list(MODELS)[index % 2])
            self.exhausted(result, reset)
            require(not result["upstream_accounts"], "exhausted account was retried for another session/model")
        require(self.snapshots() == (before_generations, before_reads),
                "known exhaustion repeated quota reads or generations before recovery")

        # A real bounded deadline, not an arbitrary sleep: the fixture turns
        # positive at this exact timestamp and CPA must stop honoring old zero.
        recovery_deadline = reset + .15
        while time.time() < recovery_deadline:
            threading.Event().wait(min(.05, recovery_deadline - time.time()))
        session = "fixture-sticky-after-reset"
        recovered = self.request("recovered", session=session)
        require(recovered["http"] == 200 and len(recovered["upstream_accounts"]) == 1,
                f"quota deadline did not restore service: {recovered}")
        owner = recovered["upstream_accounts"][0]
        other = next(account for account in TOKENS if account != owner)
        _, after_reads = self.snapshots()
        require(len(after_reads) > len(before_reads), "recovery reused an expired zero snapshot")

        # The owner has a cached positive observation. Its next POST rejects;
        # a forced quota refresh must convert generic 429 into account exhaustion.
        long_reset = time.time() + 120
        self.set_quota([owner], 0, long_reset)
        switched = self.request("quota_failover", session=session)
        require(switched["http"] == 200 and switched["upstream_accounts"] == [owner, other],
                f"generic upstream 429 did not switch once to the healthy account: {switched}")
        for index in range(3):
            retained = self.request("sticky_replacement_" + str(index), session=session,
                                    model=list(MODELS)[index % 2])
            require(retained["http"] == 200 and retained["upstream_accounts"] == [other],
                    f"replacement session returned to the exhausted account: {retained}")
        fresh = self.request("new_session_other_model", model=list(MODELS)[1])
        require(fresh["http"] == 200 and fresh["upstream_accounts"] == [other],
                f"different model bypassed account-wide exhaustion: {fresh}")

        # With the replacement also exhausted, neither new sessions nor a burst
        # may issue more generations until the advertised reset.
        self.set_quota([other], 0, long_reset)
        final = self.request("replacement_exhausted", session=session)
        self.exhausted(final, long_reset)
        require(final["upstream_accounts"] == [other], f"already exhausted owner was revisited: {final}")
        before_generations, before_reads = self.snapshots()
        with ThreadPoolExecutor(max_workers=12) as pool:
            futures = [pool.submit(self.request, "blocked_parallel_" + str(i), model=list(MODELS)[i % 2])
                       for i in range(12)]
            blocked = [future.result(timeout=15) for future in futures]
        for result in blocked:
            self.exhausted(result, long_reset)
            require(not result["upstream_accounts"], "parallel requests reached an exhausted account")
        require(self.snapshots() == (before_generations, before_reads),
                "all-exhausted burst performed extra generation or quota requests")
        generations, quota_reads = self.snapshots()
        attempts = Counter(g["request"] for g in generations)
        require(attempts["quota_failover"] == 2 and all(count <= 2 for count in attempts.values()),
                "a request exceeded the two-account failover bound")
        return {"passed": True, "initial_zero_generation_count": 0,
                "deadline_recovery": True, "generic_429_forced_quota_refresh": True,
                "account_wide_cross_model_gate": True, "sticky_replacement": True,
                "all_exhausted_parallel_requests": len(blocked), "blocked_generation_count": 0,
                "blocked_extra_quota_reads": 0, "upstream_generations": len(generations),
                "quota_reads": Counter(quota_reads), "requests": self.reports}


class ParallelFailoverSuite(Suite):
    def run(self):
        self.set_quota(TOKENS, 1, time.time() + 3600)
        session = "fixture-twelve-simultaneous-failovers"
        warm = self.request("parallel_warm", session=session)
        require(warm["http"] == 200 and len(warm["upstream_accounts"]) == 1,
                "initial sticky account was not established")
        owner = warm["upstream_accounts"][0]
        other = next(account for account in TOKENS if account != owner)
        self.set_quota([owner], 0, time.time() + 120)
        self.fixture.reject_account = owner
        self.fixture.reject_barrier = threading.Barrier(12)
        with ThreadPoolExecutor(max_workers=12) as pool:
            futures = [pool.submit(self.request, "parallel_switch_" + str(i), session=session)
                       for i in range(12)]
            results = [future.result(timeout=20) for future in futures]
        for result in results:
            require(result["http"] == 200 and result["upstream_accounts"] == [owner, other],
                    f"a simultaneous rejected request was lost or replayed: {result}")
        followup = self.request("parallel_followup", session=session)
        require(followup["http"] == 200 and followup["upstream_accounts"] == [other],
                "concurrent failovers did not preserve the replacement binding")
        generations, reads = self.snapshots()
        require(Counter(reads) == Counter({owner: 2, other: 1}),
                f"concurrent 429 responses duplicated metadata probes: {reads}")
        require(len(generations) == 26, "successful requests were replayed")
        return {"passed": True, "parallel_requests": 12, "successful_failovers": 12,
                "every_request_rejected_by_original_account": True,
                "every_request_completed_by_replacement_account": True,
                "max_attempts_per_request": 2, "sticky_replacement": True,
                "upstream_generations": len(generations), "quota_reads": Counter(reads),
                "requests": self.reports}


class ThreeAccountSuite(Suite):
    def __init__(self, port, fixture, max_retry_credentials):
        super().__init__(port, fixture)
        self.max_retry_credentials = max_retry_credentials

    def run(self):
        reset = time.time() + 120
        self.set_quota(["account_A", "account_B"], 0, reset)
        self.set_quota(["account_C"], 1, time.time() + 3600)
        session = "fixture-three-account-sticky"
        first = self.request("three_account_initial", session=session)
        generations, reads = self.snapshots()
        if self.max_retry_credentials == 2:
            self.exhausted(first, reset)
            require(not generations and reads == ["account_A", "account_B"],
                    f"two-credential baseline did not stop before C: {first}, reads={reads}")
            return {"passed": True, "max_retry_credentials": 2,
                    "expected_limit_failure": True, "healthy_third_account_unreached": True,
                    "upstream_generations": 0, "quota_reads": Counter(reads),
                    "requests": self.reports}
        require(first["http"] == 200 and first["upstream_accounts"] == ["account_C"],
                f"unlimited candidate traversal did not reach the healthy third account: {first}")
        require(reads == ["account_A", "account_B", "account_C"],
                f"priority order or once-per-account inspection violated: {reads}")
        for index in range(3):
            retained = self.request("three_account_sticky_" + str(index), session=session,
                                    model=list(MODELS)[index % 2])
            require(retained["http"] == 200 and retained["upstream_accounts"] == ["account_C"],
                    f"third-account binding did not remain sticky: {retained}")
        generations, reads = self.snapshots()
        require(reads == ["account_A", "account_B", "account_C"],
                "sticky follow-ups repeated a quota inspection")
        require(all(row["account"] == "account_C" for row in generations)
                and len(generations) == len(self.reports),
                "an exhausted account generated or a successful request was replayed")
        return {"passed": True, "max_retry_credentials": 0, "request_retry": 0,
                "credential_priorities": {"account_A": 300, "account_B": 200, "account_C": 100},
                "healthy_third_account_reached": True, "sticky_third_account": True,
                "each_account_inspected_once": True, "only_healthy_account_generated": True,
                "upstream_generations": len(generations), "quota_reads": Counter(reads),
                "requests": self.reports}


def run(cpa, plugin, *, three_account_pool=False, parallel_failover=False, max_retry_credentials=2,
        fixture_type=Fixture, suite_type=None):
    with tempfile.TemporaryDirectory(prefix="cpa-bps-quota-failover-") as workdir:
        root = pathlib.Path(workdir)
        auths, plugins = root / "auths", root / "plugins"
        auths.mkdir(mode=0o700)
        plugins.mkdir(mode=0o700)
        shutil.copy2(plugin, plugins / ("cpa-bps" + plugin.suffix))
        tokens = {**TOKENS, "account_C": "fixture-quota-token-C"} if three_account_pool else TOKENS
        token_envs = {label: "CPA_BPS_QUOTA_" + label[-1] for label in tokens}
        with ConcurrentFixtureServer((LOOPBACK, 0), fixture_type) as fixture:
            fixture.tokens = tokens
            fixture.lock = threading.Lock()
            fixture.generations, fixture.quota_reads = [], []
            fixture.accounts = {a: {"remaining": 0, "reset": time.time() + 3600, "auto_restore": False}
                                for a in tokens}
            thread = threading.Thread(target=fixture.serve_forever, daemon=True)
            thread.start()
            origin = f"http://{LOOPBACK}:{fixture.server_port}"
            for label, variable in token_envs.items():
                auth = {"type": "cpa-bps", "label": label, "token_env": variable, "upstream_origin": origin}
                if three_account_pool:
                    auth["priority"] = {"account_A": 300, "account_B": 200, "account_C": 100}[label]
                (auths / (label + ".json")).write_text(json.dumps(auth))
            port, management_key = reserve_port(), secrets.token_hex(24)
            cfg = {
                "host": LOOPBACK, "port": port, "auth-dir": str(auths), "api-keys": [API_KEY],
                "management": {"secret-key": management_key, "disable-control-panel": True,
                               "disable-auto-update-panel": True},
                "routing": {"strategy": "round-robin", "session-affinity": True,
                            "session-affinity-ttl": "24h", "session-affinity-rebind-on": "any",
                            "retry": {"request-retry": 0, "max-retry-credentials": max_retry_credentials, "max-retry-interval": 0}},
                "request-retry": 0, "max-retry-credentials": max_retry_credentials, "max-retry-interval": 0,
                "plugins": {"enabled": True, "dir": str(plugins), "configs": {"cpa-bps": {
                    "enabled": True, "responses_url": origin + "/v1/responses",
                    "quota_url": origin + "/v1/quota",
                    "models": [{"name": upstream, "alias": public} for public, upstream in MODELS.items()],
                    "account_concurrency_limit": 12,
                }}},
            }
            config_path = root / "config.yaml"
            config_path.write_text(json.dumps(cfg))
            env = {"XDG_CONFIG_HOME": workdir, "XDG_CACHE_HOME": workdir, "TMPDIR": workdir,
                   "HTTP_PROXY": origin, "HTTPS_PROXY": origin, "NO_PROXY": LOOPBACK,
                   **{token_envs[label]: token for label, token in tokens.items()}}
            log_path, process, result = root / "cpa.log", None, None
            suite = (suite_type(port, fixture) if suite_type is not None
                     else ThreeAccountSuite(port, fixture, max_retry_credentials) if three_account_pool
                     else ParallelFailoverSuite(port, fixture) if parallel_failover else Suite(port, fixture))
            try:
                with log_path.open("wb") as log:
                    process = subprocess.Popen([str(cpa), "-local-model", "-config", str(config_path)],
                                               cwd=root, env=env, stdout=log, stderr=log)
                    try:
                        ready(process, port)
                        result = suite.run()
                    except Exception as error:
                        log.flush()
                        generations, reads = suite.snapshots()
                        raise RuntimeError(f"{error}\nObserved generations: {json.dumps(generations)}\n"
                                           f"Quota reads: {json.dumps(reads)}\n"
                                           f"Requests: {json.dumps(suite.reports)}\n"
                                           f"CPA diagnostic log:\n{read_diagnostics(log_path)}") from error
                    finally:
                        if process.poll() is None:
                            process.terminate()
                        try:
                            process.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait(timeout=5)
            finally:
                fixture.shutdown()
                thread.join(timeout=5)
            result["isolated_process_cleaned_up"] = process.poll() is not None
            return result


def interrupted(*_):
    raise KeyboardInterrupt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=pathlib.Path, required=True)
    parser.add_argument("--plugin", type=pathlib.Path, required=True)
    parser.add_argument("--evidence", type=pathlib.Path)
    parser.add_argument("--three-account-pool", action="store_true",
                        help="compare a two-candidate cap against complete traversal of a three-account pool")
    parser.add_argument("--parallel-failover", action="store_true",
                        help="force twelve simultaneous requests to receive 429 on one account and complete on another")
    args = parser.parse_args()
    cpa, plugin = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    if args.three_account_pool and args.parallel_failover:
        parser.error("choose one scenario")
    if args.evidence is not None and args.evidence.exists():
        parser.error("evidence file exists; choose a new path")
    evidence = {"started_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
                "synthetic_loopback_only": True, "production_modified": False,
                "plugin_sha256": hashlib.sha256(plugin.read_bytes()).hexdigest(),
                "cpa_sha256": hashlib.sha256(cpa.read_bytes()).hexdigest()}
    previous = signal.signal(signal.SIGTERM, interrupted)
    try:
        if args.three_account_pool:
            evidence["mode"] = "three_account_pool"
            evidence["two_candidate_baseline"] = run(cpa, plugin, three_account_pool=True, max_retry_credentials=2)
            evidence["full_pool"] = run(cpa, plugin, three_account_pool=True, max_retry_credentials=0)
            evidence["passed"] = all(evidence[key]["passed"] and evidence[key]["isolated_process_cleaned_up"]
                                     for key in ("two_candidate_baseline", "full_pool"))
        elif args.parallel_failover:
            evidence["mode"] = "parallel_failover"
            evidence.update(run(cpa, plugin, parallel_failover=True, max_retry_credentials=0))
        else:
            evidence.update(run(cpa, plugin))
    finally:
        signal.signal(signal.SIGTERM, previous)
    if args.evidence:
        args.evidence.parent.mkdir(parents=True, exist_ok=True)
        args.evidence.write_text(json.dumps(evidence, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(evidence, ensure_ascii=False))


if __name__ == "__main__":
    main()
