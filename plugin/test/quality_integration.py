#!/usr/bin/env python3
"""Verify quality scheduling through an isolated native CPA host and loopback API.

Only temporary synthetic credentials are used. The real one-minute schedule is
observed without making any management HTTP requests during the waiting window.
The suite then restarts its own CPA process, pauses the plan, and waits another
full interval to prove that no additional model requests are sent.
"""

import argparse
from contextlib import closing
from datetime import datetime, timezone
import hashlib
import http.client
import http.server
import json
import pathlib
import secrets
import shutil
import signal
import stat
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlencode

from integration import LOOPBACK, event_bytes, read_diagnostics, require, reserve_port


API_KEY = "fixture-quality-client"
TOKENS = {"A": "fixture-quality-unused-A", "B1": "fixture-quality-B-first", "B2": "fixture-quality-B-rotated"}
TOKEN_ENVS = {key: "CPA_BPS_QUALITY_" + key for key in TOKENS}
MODELS = {"fixture-candy-pass": "21", "fixture-candy-fail": "29"}
ALIASES = [name + "-excel" for name in MODELS]
MAX_BODY = 1 << 20
MANAGEMENT = "/v8/management"
QUALITY = "/v0/management/cpa-bps/quality/"


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
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_CONNECT(self):
        # The isolated CPA's ambient proxy denies all external update traffic.
        self.reply(403, b"{}")

    def do_GET(self):
        self.reply(404, b"{}")

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        if self.path != "/v1/responses" or not 0 < length <= MAX_BODY:
            self.reply(400, b"{}")
            return
        payload = json.loads(self.rfile.read(length))
        token_label = next((label for label, token in TOKENS.items()
                            if self.headers.get("Authorization") == "Bearer " + token), None)
        model = payload.get("model")
        valid = (token_label in {"B1", "B2"} and model in MODELS and payload.get("stream") is True
                 and not payload.get("tools") and payload.get("tool_choice") == "none"
                 and "expected_answer" not in payload)
        with self.server.condition:
            self.server.requests.append({"model": model, "token_generation": token_label,
                                         "valid_request": valid, "at": time.monotonic()})
            ordinal = len(self.server.requests)
            self.server.condition.notify_all()
        if not valid:
            self.reply(400, b'{"error":{"code":"invalid_fixture_request"}}')
            return
        if ordinal == 1:
            self.server.first_started.set()
            if not self.server.release_first.wait(15):
                self.reply(504, b'{"error":{"code":"fixture_gate_timeout"}}')
                return
        answer = MODELS[model]
        response = {
            "id": "fixture-quality-response-" + str(ordinal), "object": "response", "status": "completed",
            "model": model, "output": [{"type": "message", "role": "assistant", "status": "completed",
                "content": [{"type": "output_text", "text": answer, "annotations": []}]}],
            "usage": {"input_tokens": 17, "output_tokens": 2, "total_tokens": 19},
        }
        body = event_bytes({"type": "response.created", "response": {"id": response["id"], "status": "in_progress"}})
        body += event_bytes({"type": "response.output_text.delta", "delta": answer})
        body += event_bytes({"type": "response.completed", "response": response})
        self.reply(200, body, "text/event-stream; charset=utf-8")


def call(port, key, method, path, data=None, timeout=5):
    headers = {"Authorization": "Bearer " + key}
    body = None
    if data is not None:
        body = json.dumps(data).encode()
        headers["Content-Type"] = "application/json"
    with closing(http.client.HTTPConnection(LOOPBACK, port, timeout=timeout)) as client:
        client.request(method, path, body, headers)
        with client.getresponse() as reply:
            raw = reply.read(MAX_BODY + 1)
            require(len(raw) <= MAX_BODY, "quality fixture response exceeded limit")
            return reply.status, json.loads(raw) if raw else {}


def quality_call(port, key, method, endpoint, index, data=None, expected=200):
    status, result = call(port, key, method, QUALITY + endpoint + "?" + urlencode({"auth_index": index}), data)
    require(status == expected, f"quality {method} {endpoint}: expected {expected}, got {status}: {result}")
    return result


def ready(process, port, key):
    deadline = time.monotonic() + 20
    while process.poll() is None and time.monotonic() < deadline:
        try:
            status, result = call(port, API_KEY, "GET", "/v1/models", timeout=1)
            if status == 200 and set(ALIASES).issubset({item["id"] for item in result.get("data", [])}):
                status, listing = call(port, key, "GET", MANAGEMENT + "/credentials", timeout=1)
                rows = listing.get("files", [])
                if status == 200 and len(rows) == 2:
                    return {row["name"]: row for row in rows}
        except (OSError, ValueError, http.client.HTTPException):
            pass
        time.sleep(.1)
    raise RuntimeError("isolated native quality CPA did not become ready")


def stop(process):
    if process is None:
        return
    if process.poll() is None:
        process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def poll_results(port, key, index, count):
    deadline = time.monotonic() + 12
    last = {}
    while time.monotonic() < deadline:
        last = quality_call(port, key, "GET", "results", index)
        if len(last.get("results", [])) == count and not last.get("running"):
            return last
        time.sleep(.1)
    raise RuntimeError(f"native quality results did not reach {count}: {last}")


def validate_results(view, rounds):
    results = view["results"]
    require(len(results) == 2 * rounds, "wrong quality history count")
    run_ids = {result["run_id"] for result in results}
    require(len(run_ids) == rounds, "manual and scheduled runs did not have distinct IDs")
    for run_id in run_ids:
        rows = [result for result in results if result["run_id"] == run_id]
        require({result["requested_model"] for result in rows} == set(ALIASES), "multi-model results were merged")
        for result in rows:
            model = result["requested_model"].removesuffix("-excel")
            expected_status = "pass" if MODELS[model] == "21" else "answer_fail"
            require(result["status"] == expected_status and result["answer"] == MODELS[model], "wrong model answer grading")
            require(result["actual_model"] == model and result["valid_for_requested_model"]
                    and not result["model_mismatch"], "actual model attribution was lost")
            require(result["usage"] == {"input_tokens": 17, "output_tokens": 2, "total_tokens": 19}, "usage numbers lost")
            require(result["benchmark_version"] == "candy-sub2api-v1" and result["prompt"]
                    and result["expected_answer"] == "21", "benchmark snapshot missing")


def run(cpa, plugin):
    with tempfile.TemporaryDirectory(prefix="cpa-bps-quality-native-") as workdir:
        root = pathlib.Path(workdir)
        auths, plugins = root / "auths", root / "plugins"
        auths.mkdir(mode=0o700)
        plugins.mkdir(mode=0o700)
        copied_plugin = plugins / ("cpa-bps" + plugin.suffix)
        shutil.copy2(plugin, copied_plugin)
        quality_state = plugins / "quality-state.json"
        with http.server.ThreadingHTTPServer((LOOPBACK, 0), Fixture) as upstream:
            upstream.condition = threading.Condition()
            upstream.requests = []
            upstream.first_started = threading.Event()
            upstream.release_first = threading.Event()
            thread = threading.Thread(target=upstream.serve_forever, daemon=True)
            thread.start()
            origin = f"http://{LOOPBACK}:{upstream.server_port}"

            def write_auth(label, generation):
                path = auths / ("fixture-quality-" + label + ".json")
                value = {"type": "cpa-bps", "label": "Synthetic quality " + label,
                         "token_env": TOKEN_ENVS[generation], "upstream_origin": origin}
                temporary = auths / (path.name + ".tmp")
                temporary.write_text(json.dumps(value))
                temporary.chmod(0o600)
                temporary.replace(path)

            write_auth("A", "A")
            write_auth("B", "B1")
            port, key = reserve_port(), secrets.token_hex(24)
            require(port not in {8318, 18318} and upstream.server_port not in {8318, 18318}, "reserved service port selected")
            config = {
                "host": LOOPBACK, "port": port, "auth-dir": str(auths), "api-keys": [API_KEY],
                "management": {"secret-key": key, "disable-control-panel": True, "disable-auto-update-panel": True},
                "request-retry": 0, "max-retry-credentials": 1, "max-retry-interval": 0,
                "plugins": {"enabled": True, "dir": str(plugins), "configs": {"cpa-bps": {
                    "enabled": True, "responses_url": origin + "/v1/responses",
                    "models": [{"name": name, "alias": name + "-excel"} for name in MODELS],
                    "account_concurrency_limit": 1, "quality_state_path": str(quality_state),
                }}},
            }
            config_path = root / "config.yaml"
            config_path.write_text(json.dumps(config))
            env = {"XDG_CONFIG_HOME": workdir, "XDG_CACHE_HOME": workdir, "TMPDIR": workdir,
                   "HTTP_PROXY": origin, "HTTPS_PROXY": origin, "NO_PROXY": LOOPBACK,
                   **{TOKEN_ENVS[label]: token for label, token in TOKENS.items()}}
            process = None
            log_path = root / "cpa.log"
            evidence = {"passed": False, "synthetic_loopback_only": True,
                        "plugin_sha256": hashlib.sha256(copied_plugin.read_bytes()).hexdigest()}
            try:
                with log_path.open("ab") as log:
                    def start():
                        return subprocess.Popen([str(cpa), "-local-model", "-config", str(config_path)],
                                                cwd=root, env=env, stdout=log, stderr=log)

                    process = start()
                    accounts = ready(process, port, key)
                    index = accounts["fixture-quality-B.json"]["auth_index"]
                    default = quality_call(port, key, "GET", "config", index)
                    require(not default["config"]["enabled"] and not upstream.requests, "reading defaults sent a model request")
                    catalog = quality_call(port, key, "GET", "models", index)
                    require(catalog.get("source") == "host_registered" and
                            {item["id"] for item in catalog["models"]} == set(ALIASES) and not upstream.requests,
                            "selected credential catalog was incomplete or queried upstream")
                    plan = default["config"]
                    plan.update({"enabled": False, "models": ALIASES, "reasoning_effort": "high"})
                    before_manual = quality_call(port, key, "PUT", "config", index, plan)
                    started = time.monotonic()
                    accepted = quality_call(port, key, "POST", "run", index, {}, expected=202)
                    require(accepted.get("run_id") and time.monotonic() - started < 4, "manual run blocked instead of returning 202")
                    require(upstream.first_started.wait(10), "asynchronous manual runner did not reach loopback upstream")
                    # The HTTP request is already closed. The second model must
                    # ask the host for the latest physical credential itself.
                    write_auth("B", "B2")
                    upstream.release_first.set()
                    manual = poll_results(port, key, index, 2)
                    validate_results(manual, 1)
                    require(manual["config"] == before_manual["config"] and
                            manual["next_due"] == before_manual["next_due"] and
                            all(item.get("manual") is True for item in manual["results"]),
                            "manual execution changed periodic settings or missed provenance")
                    with upstream.condition:
                        require([row["token_generation"] for row in upstream.requests] == ["B1", "B2"],
                                "background callback reused stale credentials or another account")
                    require(stat.S_IMODE(quality_state.stat().st_mode) == 0o600, "quality state is not mode 0600")
                    raw_state = quality_state.read_text()
                    require(all(token not in raw_state for token in TOKENS.values()) and "token_env" not in raw_state,
                            "quality state persisted credential material")
                    evidence.update({"async_202": True, "fresh_background_host_callback": True,
                                     "manual_statuses": ["pass", "answer_fail"], "state_mode": "0600",
                                     "host_registered_catalog": True, "manual_preserves_periodic_plan": True})
                    print("PASS native manual quality: async 202, fresh host callback, two separate model results", flush=True)

                    plan.update({"enabled": True, "interval_minutes": 1})
                    quality_call(port, key, "PUT", "config", index, plan)
                    wait_started = time.monotonic()
                    print("Waiting for the native one-minute schedule without browser or management polling…", flush=True)
                    with upstream.condition:
                        finished = upstream.condition.wait_for(lambda: len(upstream.requests) >= 4, timeout=80)
                    elapsed = time.monotonic() - wait_started
                    require(finished and elapsed >= 55, "schedule did not fire once at its real due time")
                    scheduled = poll_results(port, key, index, 4)
                    validate_results(scheduled, 2)
                    with upstream.condition:
                        require(len(upstream.requests) == 4 and all(row["valid_request"] for row in upstream.requests),
                                "scheduler replayed, sent tools, or used the wrong account")
                    evidence.update({"automatic_without_browser": True, "automatic_wait_seconds": round(elapsed, 3)})
                    print("PASS native scheduler: one due round, two requests, no browser session", flush=True)

                    next_due = scheduled["next_due"]
                    history_ids = {row["id"] for row in scheduled["results"]}
                    stop(process)
                    process = start()
                    restarted_accounts = ready(process, port, key)
                    require(restarted_accounts["fixture-quality-B.json"]["auth_index"] == index, "synthetic account index changed on restart")
                    restored = quality_call(port, key, "GET", "results", index)
                    require(restored["next_due"] == next_due and {row["id"] for row in restored["results"]} == history_ids,
                            "restart lost history or changed the next due time")
                    require(len(upstream.requests) == 4, "restart replayed a completed round")
                    plan["enabled"] = False
                    paused = quality_call(port, key, "PUT", "config", index, plan)
                    require(not paused["config"]["enabled"], "quality plan did not pause")
                    print("Waiting across another full interval to verify the paused native plan stays quiet…", flush=True)
                    with upstream.condition:
                        unexpected = upstream.condition.wait_for(lambda: len(upstream.requests) != 4, timeout=70)
                    require(not unexpected, "paused plan sent another upstream request")
                    other = quality_call(port, key, "GET", "results", accounts["fixture-quality-A.json"]["auth_index"])
                    require(not other["results"] and not other["config"]["enabled"], "unselected account was observed or enabled")
                    final = quality_call(port, key, "GET", "results", index)
                    require(len(final["results"]) == 4, "pause changed history")
                    require(stat.S_IMODE(quality_state.stat().st_mode) == 0o600, "state permissions changed")
                    evidence.update({"restart_preserved_history_and_next_due": True, "restart_replayed_requests": 0,
                                     "paused_full_interval_no_requests": True, "unselected_account_requests": 0,
                                     "total_generations": 4, "passed": True})
                    print("PASS native restart and pause: history preserved, no replay, no new requests after pause", flush=True)
            except Exception as error:
                raise RuntimeError(f"{error}\nCPA diagnostic log:\n{read_diagnostics(log_path)}") from error
            finally:
                upstream.release_first.set()
                stop(process)
                evidence["isolated_process_cleaned_up"] = process is None or process.poll() is not None
                upstream.shutdown()
                thread.join(timeout=5)
            return evidence


def interrupted(*_):
    raise KeyboardInterrupt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=pathlib.Path, required=True)
    parser.add_argument("--plugin", type=pathlib.Path, required=True)
    parser.add_argument("--evidence", type=pathlib.Path)
    args = parser.parse_args()
    if args.evidence is not None and args.evidence.exists():
        parser.error("evidence already exists; choose a new path")
    cpa, plugin = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    previous = signal.signal(signal.SIGTERM, interrupted)
    evidence = {"started_at": datetime.now(timezone.utc).isoformat(timespec="seconds"), "passed": False}
    try:
        evidence.update(run(cpa, plugin))
        print(json.dumps(evidence, ensure_ascii=False), flush=True)
    finally:
        signal.signal(signal.SIGTERM, previous)
        evidence["finished_at"] = datetime.now(timezone.utc).isoformat(timespec="seconds")
        if args.evidence is not None:
            args.evidence.parent.mkdir(parents=True, exist_ok=True)
            with args.evidence.open("x", encoding="utf-8") as output:
                json.dump(evidence, output, ensure_ascii=False, indent=2)
                output.write("\n")


if __name__ == "__main__":
    main()
