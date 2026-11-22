#!/usr/bin/env python3
"""Real native-ABI client disconnect checks against a stalled loopback upstream."""

import argparse
import hashlib
import http.server
import json
import pathlib
import secrets
import shutil
import socket
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlencode

import sticky_session_integration as sticky
from integration import LOOPBACK, read_diagnostics, require, reserve_port


class Fixture(sticky.Fixture):
    def do_POST(self):
        payload = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        marker = str(payload.get("input", ""))
        with self.server.lock:
            self.server.requests.append(marker)
        if marker == "recover":
            response = {"id": "resp_recover", "status": "completed", "model": sticky.UPSTREAM_MODEL,
                        "output": [{"type": "message", "role": "assistant", "content": [
                            {"type": "output_text", "text": "RECOVERED"}]}]}
            self.reply(200, json.dumps(response).encode())
            return
        if payload.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b'event: response.created\ndata: {"type":"response.created","response":{"id":"resp_hold","status":"in_progress"}}\n\n')
            self.wfile.flush()
        self.server.entered.set()
        self.connection.settimeout(8)
        try:
            if self.connection.recv(1) == b"":
                self.server.disconnected.set()
        except (ConnectionResetError, BrokenPipeError):
            self.server.disconnected.set()
        except TimeoutError:
            pass


def connect(port, marker, stream=False):
    body = json.dumps({"model": sticky.MODEL, "input": marker, "stream": stream}).encode()
    client = socket.create_connection((LOOPBACK, port), timeout=3)
    client.sendall((f"POST /v1/responses HTTP/1.1\r\nHost: {LOOPBACK}\r\n"
                    f"Authorization: Bearer {sticky.API_KEY}\r\nContent-Type: application/json\r\n"
                    f"Content-Length: {len(body)}\r\n\r\n").encode() + body)
    return client


def disconnect(client):
    try:
        client.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass
    client.close()


def run(cpa, plugin):
    with tempfile.TemporaryDirectory(prefix="cpa-bps-cancel-") as work:
        root = pathlib.Path(work)
        auths, plugins = root / "auths", root / "plugins"
        auths.mkdir(); plugins.mkdir()
        shutil.copy2(plugin, plugins / ("cpa-bps" + plugin.suffix))
        with sticky.ConcurrentFixtureServer((LOOPBACK, 0), Fixture) as upstream:
            upstream.lock = threading.Lock()
            upstream.requests = []
            upstream.entered = threading.Event()
            upstream.disconnected = threading.Event()
            worker = threading.Thread(target=upstream.serve_forever, daemon=True)
            worker.start()
            origin = f"http://{LOOPBACK}:{upstream.server_port}"
            (auths / "account_A.json").write_text(json.dumps({"type": "cpa-bps", "token_env": "CPA_BPS_CANCEL_TOKEN", "upstream_origin": origin}))
            port, key = reserve_port(), secrets.token_hex(24)
            config = {"host": LOOPBACK, "port": port, "auth-dir": str(auths), "api-keys": [sticky.API_KEY],
                      "management": {"secret-key": key, "disable-control-panel": True, "disable-auto-update-panel": True},
                      "routing": {"retry": {"request-retry": 0, "max-retry-credentials": 1}},
                      "plugins": {"enabled": True, "dir": str(plugins), "configs": {"cpa-bps": {
                          "enabled": True, "responses_url": origin + "/v1/responses", "account_concurrency_limit": 1,
                          "models": [{"name": sticky.UPSTREAM_MODEL, "alias": sticky.MODEL}]}}}}
            path = root / "config.yaml"
            path.write_text(json.dumps(config))
            env = {"XDG_CONFIG_HOME": work, "XDG_CACHE_HOME": work, "TMPDIR": work,
                   "HTTP_PROXY": origin, "HTTPS_PROXY": origin, "NO_PROXY": LOOPBACK,
                   "CPA_BPS_CANCEL_TOKEN": sticky.TOKENS["account_A"]}
            clients = []
            reports = []
            log_path = root / "cpa.log"
            with log_path.open("wb") as log:
                process = subprocess.Popen([str(cpa), "-local-model", "-config", str(path)], cwd=root, env=env, stdout=log, stderr=log)
                try:
                    sticky.ready(process, port)
                    status, _, raw = sticky.call(port, "GET", "/v8/management/credentials", key=key)
                    require(status == 200, "credential listing failed")
                    auth_index = json.loads(raw)["files"][0]["auth_index"]
                    route = "/v0/management/cpa-bps/concurrency?" + urlencode({"auth_index": auth_index})
                    def state(active, waiting):
                        deadline = time.monotonic() + 3
                        while time.monotonic() < deadline:
                            status, _, raw = sticky.call(port, "GET", route, key=key)
                            require(status == 200, "concurrency state unavailable")
                            observed = json.loads(raw)
                            if observed["active"] == active and observed["waiting"] == waiting:
                                return observed
                            threading.Event().wait(.02)
                        raise RuntimeError(f"slots failed to settle: expected active={active} waiting={waiting}, got {observed}")
                    for stream in (False, True):
                        upstream.entered.clear(); upstream.disconnected.clear()
                        owner = connect(port, "hold_stream" if stream else "hold_json", stream)
                        clients.append(owner)
                        require(upstream.entered.wait(3), "owner never reached upstream")
                        state(1, 0)
                        queued = connect(port, "queued")
                        clients.append(queued)
                        state(1, 1)
                        start = time.monotonic()
                        disconnect(queued); clients.remove(queued)
                        state(1, 0)
                        queued_elapsed = time.monotonic() - start
                        require("queued" not in upstream.requests, "canceled queued work reached upstream")
                        if stream:
                            data = b""
                            while b"response.created" not in data:
                                data += owner.recv(4096)
                        start = time.monotonic()
                        disconnect(owner); clients.remove(owner)
                        require(upstream.disconnected.wait(3), "client cancellation did not close upstream connection")
                        state(0, 0)
                        active_elapsed = time.monotonic() - start
                        status, _, raw = sticky.call(port, "POST", "/v1/responses", data={"model": sticky.MODEL, "input": "recover", "stream": False})
                        require(status == 200 and b"RECOVERED" in raw, "subsequent request failed after cancellation")
                        state(0, 0)
                        reports.append({"stream": stream, "queued_cancel_seconds": round(queued_elapsed, 4),
                                        "active_cancel_seconds": round(active_elapsed, 4), "queue_drained": True,
                                        "upstream_disconnected": True, "subsequent_request_succeeded": True})
                    require(len(upstream.requests) == 4, "cancellation caused generation or replay")
                    return {"passed": True, "synthetic_loopback_only": True, "runs": reports,
                            "upstream_requests": len(upstream.requests), "queued_requests_sent": 0}
                except Exception as error:
                    log.flush()
                    raise RuntimeError(f"{error}\n{read_diagnostics(log_path)}") from error
                finally:
                    for client in clients:
                        disconnect(client)
                    process.terminate()
                    try: process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill(); process.wait(timeout=5)
                    upstream.shutdown(); worker.join(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=pathlib.Path, required=True)
    parser.add_argument("--plugin", type=pathlib.Path, required=True)
    parser.add_argument("--evidence", type=pathlib.Path)
    args = parser.parse_args()
    cpa, plugin = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    if args.evidence and args.evidence.exists(): parser.error("evidence already exists")
    result = run(cpa, plugin)
    result.update({"cpa_sha256": hashlib.sha256(cpa.read_bytes()).hexdigest(), "plugin_sha256": hashlib.sha256(plugin.read_bytes()).hexdigest()})
    if args.evidence:
        args.evidence.parent.mkdir(parents=True, exist_ok=True)
        args.evidence.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result))


if __name__ == "__main__":
    main()
