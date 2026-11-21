#!/usr/bin/env python3
"""Smoke-test a rebuilt native plugin with CPA and a synthetic loopback upstream."""

import argparse
from contextlib import closing
import http.client
import http.server
import json
import pathlib
import shutil
import socket
import subprocess
import tempfile
import threading
import time


LOOPBACK = "127.0.0.1"
API_KEY = "fixture-client-key"
UPSTREAM_TOKEN = "fixture-upstream-token"
TOKEN_ENV = "CPA_BPS_TEST_TOKEN"
MODEL = "mock-bps"
UPSTREAM_MODEL = "mock-upstream"
DISCOVERED_MODEL = UPSTREAM_MODEL + "-excel"
MARKER = "CPA_BPS_NATIVE_SMOKE"
RESPONSE_BODY = {
    "id": "resp_fixture",
    "object": "response",
    "status": "completed",
    "model": UPSTREAM_MODEL,
    "output": [{
        "id": "msg_fixture",
        "type": "message",
        "role": "assistant",
        "status": "completed",
        "content": [{"type": "output_text", "text": MARKER, "annotations": []}],
    }],
    "usage": {"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def event_bytes(event):
    return f"event: {event['type']}\ndata: {json.dumps(event)}\n\n".encode()


class Upstream(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_CONNECT(self):
        # CPA background updater HTTP clients receive a loopback proxy denial.
        self.reply(403, b"{}")

    def do_GET(self):
        if self.path != "/v1/models":
            self.reply(404, b"{}")
            return
        authorized = self.headers.get("Authorization") == "Bearer " + UPSTREAM_TOKEN
        with self.server.requests_lock:
            self.server.catalog_requests.append(authorized)
        if authorized:
            self.reply(200, json.dumps({"data": [{"id": UPSTREAM_MODEL}]}).encode())
        else:
            self.reply(401, b"{}")

    def reply(self, status, body):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path != "/v1/responses":
            self.reply(404, b"{}")
            return
        authorized = self.headers.get("Authorization") == "Bearer " + UPSTREAM_TOKEN
        payload = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        with self.server.requests_lock:
            self.server.requests.append((authorized, payload))
        if not authorized:
            self.reply(401, b"{}")
            return

        first = event_bytes({"type": "response.created", "response": {
            "id": "resp_fixture", "object": "response", "status": "in_progress",
        }}) + event_bytes({
            "type": "response.output_text.delta", "item_id": "msg_fixture",
            "output_index": 0, "content_index": 0, "delta": MARKER,
        })
        end = event_bytes({"type": "response.completed", "response": RESPONSE_BODY})
        input_json = json.dumps(payload.get("input"))
        early_eof = "fixture-early-eof" in input_json
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream; charset=utf-8")
        self.send_header("Content-Length", str(len(first) if early_eof else len(first) + len(end)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(first)
        self.wfile.flush()
        if early_eof:
            return
        if "fixture-stream" in input_json:
            self.server.stream_gate_used.set()
            if not self.server.allow_completion.wait(10):
                self.close_connection = True
                return
        self.wfile.write(end)


def request(port, path, data=None, observe_stream=None, *, timeout=10):
    headers = {"Authorization": "Bearer " + API_KEY}
    body = None
    if data is not None:
        headers["Content-Type"] = "application/json"
        body = json.dumps(data).encode()
    if observe_stream is not None:
        headers["Accept"] = "text/event-stream"
    with closing(http.client.HTTPConnection(LOOPBACK, port, timeout=timeout)) as client:
        client.request("GET" if data is None else "POST", path, body, headers)
        with client.getresponse() as result:
            content_type = result.getheader("Content-Type", "")
            if observe_stream is None or result.status != 200:
                return result.status, content_type, result.read()
            events = []
            while line := result.readline():
                if not line.startswith(b"data:"):
                    continue
                event_data = line[5:].strip()
                if not event_data or event_data == b"[DONE]":
                    continue
                event = json.loads(event_data)
                events.append(event)
                observe_stream(event)
            return result.status, content_type, events


def reserve_port():
    with socket.socket() as sock:
        sock.bind((LOOPBACK, 0))
        return sock.getsockname()[1]


def start_ready(process, port, model):
    deadline = time.monotonic() + 20
    observed = "no HTTP response"
    while time.monotonic() < deadline and process.poll() is None:
        try:
            status, _, body = request(port, "/v1/models", timeout=1)
            observed = f"HTTP {status}"
            if status == 200:
                ids = [model["id"] for model in json.loads(body)["data"]]
                if model in ids:
                    return
                observed += f" with {len(ids)} models (first five: {ids[:5]})"
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(0.2)
    if process.returncode is not None:
        raise RuntimeError(f"CPA exited during startup with code {process.returncode}")
    raise RuntimeError(f"CPA did not expose {model!r} on /v1/models; observed {observed}")


def read_diagnostics(path):
    lines = [line for line in path.read_text(errors="replace").splitlines()
             if not ("[gin_logger.go:" in line and '"/v1/models"' in line)]
    if len(lines) > 24:
        lines = lines[:12] + ["[additional log lines omitted]"] + lines[-12:]
    return "\n".join(lines)


def smoke(process, port, upstream, discovery):
    model = DISCOVERED_MODEL if discovery else MODEL
    start_ready(process, port, model)
    status, kind, body = request(port, "/v1/responses", {
        "model": model, "input": "fixture-json", "stream": False,
    })
    require(status == 200, f"nonstream request failed with HTTP {status}: {body[:600]!r}")
    require("application/json" in kind, f"expected JSON, got {kind}")
    response = json.loads(body)
    require(response.get("status") == "completed", "nonstream completed status missing")
    require(response["output"][0]["content"][0]["text"] == MARKER, "nonstream output mismatch")

    def release_after_delta(event):
        if event.get("type") == "response.output_text.delta" and event.get("delta") == MARKER:
            upstream.allow_completion.set()

    status, kind, events = request(port, "/v1/responses", {
        "model": model, "input": "fixture-stream", "stream": True, "service_tier": "default",
    }, release_after_delta)
    require(status == 200, f"stream failed with HTTP {status}: {events!r}")
    require("text/event-stream" in kind, f"expected SSE, got {kind}")
    require(upstream.stream_gate_used.is_set(), "fixture did not hold back its terminal event")
    require(upstream.allow_completion.is_set(), "client did not receive an incremental delta")
    completed = [event for event in events if event.get("type") == "response.completed"]
    require(len(completed) == 1 and completed[0]["response"]["status"] == "completed",
            "stream must receive one completed event")

    status, kind, events = request(port, "/v1/responses", {
        "model": model, "input": "fixture-early-eof", "stream": True,
    }, lambda event: None)
    require(status == 200 and "text/event-stream" in kind, "early-EOF stream was not accepted")
    event_types = [event.get("type") for event in events]
    require("response.output_text.delta" in event_types, f"early-EOF stream lost its delta: {event_types}")
    require(not {"response.completed", "response.done"}.intersection(event_types),
            f"early-EOF stream fabricated success: {event_types}")
    require(event_types[-1] in {"error", "response.error", "response.failed"},
            f"early-EOF stream did not emit a terminal failure: {event_types}")

    status, _, body = request(port, "/v1/responses", {
        "model": model, "input": "fixture", "previous_response_id": "resp_fixture",
    })
    require(status == 400 and b"bps_requires_full_history" in body,
            f"history reference must fail explicitly: HTTP {status} {body[:600]!r}")

    status, _, body = request(port, "/v1/responses/compact", {
        "model": model, "input": "fixture",
    })
    require(status == 400 and b"unsupported_executor_route" in body,
            f"compact route must fail explicitly: HTTP {status} {body[:600]!r}")

    with upstream.requests_lock:
        requests = list(upstream.requests)
        catalog_requests = list(upstream.catalog_requests)
    require(len(requests) == 3, f"expected only three upstream generations, got {len(requests)}")
    require(sum("fixture-early-eof" in json.dumps(payload.get("input")) for _, payload in requests) == 1,
            "early-EOF generation was attempted more or less than once")
    for authorized, payload in requests:
        require(authorized, "synthetic backend did not receive its fixture token")
        require(payload.get("model") == UPSTREAM_MODEL, "alias was not mapped to the upstream model")
        require(payload.get("service_tier") == "priority", "Fast mode did not set priority tier")
    if discovery:
        require(catalog_requests and all(catalog_requests), "no authenticated model discovery request observed")
    else:
        require(not catalog_requests, "manual model configuration unexpectedly queried the upstream catalog")
    source = "discovery" if discovery else "manual model"
    print(f"PASS {source}: registration, aggregation, incremental SSE, early EOF, route errors, alias and Fast mode")


def run(cpa, plugin, discovery):
    with tempfile.TemporaryDirectory(prefix="cpa-bps-integration-") as workdir:
        root = pathlib.Path(workdir)
        auths, plugins = root / "auths", root / "plugins"
        auths.mkdir()
        plugins.mkdir()
        (auths / "fixture.json").write_text(json.dumps({"type": "cpa-bps", "token_env": TOKEN_ENV}))
        shutil.copy2(plugin, plugins / ("cpa-bps" + plugin.suffix))

        with http.server.ThreadingHTTPServer((LOOPBACK, 0), Upstream) as upstream:
            upstream.allow_completion = threading.Event()
            upstream.stream_gate_used = threading.Event()
            upstream.requests = []
            upstream.catalog_requests = []
            upstream.requests_lock = threading.Lock()
            thread = threading.Thread(target=upstream.serve_forever, daemon=True)
            thread.start()
            try:
                port = reserve_port()
                upstream_url = f"http://{LOOPBACK}:{upstream.server_port}"
                config_path = root / "config.yaml"
                config_path.write_text(json.dumps({
                    "host": LOOPBACK,
                    "port": port,
                    "auth-dir": str(auths),
                    "api-keys": [API_KEY],
                    "remote-management": {"disable-control-panel": True, "disable-auto-update-panel": True},
                    "request-retry": 0,
                    "plugins": {"enabled": True, "dir": str(plugins), "configs": {
                        "cpa-bps": {
                            "enabled": True,
                            "responses_url": upstream_url + "/v1/responses",
                            "models_url": upstream_url + "/v1/models",
                            "models": [] if discovery else [{"name": UPSTREAM_MODEL, "alias": MODEL}],
                            "fast_mode": True,
                            "account_concurrency_limit": 1,
                        },
                    }},
                }))
                env = {
                    "HOME": workdir,
                    "XDG_CONFIG_HOME": workdir,
                    "XDG_CACHE_HOME": workdir,
                    "TMPDIR": workdir,
                    TOKEN_ENV: UPSTREAM_TOKEN,
                    "HTTP_PROXY": upstream_url,
                    "HTTPS_PROXY": upstream_url,
                    "NO_PROXY": LOOPBACK,
                }
                log_path = root / "cpa.log"
                with log_path.open("wb") as log:
                    process = subprocess.Popen([str(cpa), "-local-model", "-config", str(config_path)],
                                               cwd=root, env=env, stdout=log, stderr=log)
                    try:
                        smoke(process, port, upstream, discovery)
                    except Exception as error:
                        log.flush()
                        details = read_diagnostics(log_path)
                        raise RuntimeError(f"{error}\nCPA diagnostic log:\n{details}") from error
                    finally:
                        upstream.allow_completion.set()
                        if process.poll() is None:
                            process.terminate()
                        try:
                            process.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait(timeout=5)
            finally:
                upstream.allow_completion.set()
                upstream.shutdown()
                thread.join(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", required=True, type=pathlib.Path, help="CPA executable for this platform")
    parser.add_argument("--plugin", required=True, type=pathlib.Path, help="rebuilt native shared library")
    args = parser.parse_args()
    cpa = args.cpa.resolve(strict=True)
    plugin = args.plugin.resolve(strict=True)
    run(cpa, plugin, discovery=False)
    run(cpa, plugin, discovery=True)


if __name__ == "__main__":
    main()
