#!/usr/bin/env python3
"""Verify one real Responses request through isolated CPA and an authorized gateway."""

import argparse
from collections import Counter
from contextlib import closing, nullcontext
from datetime import datetime, timedelta, timezone
import http.client
import http.server
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlsplit


LOOPBACK = "127.0.0.1"
TOKEN_ENV = "CPA_BPS_LIVE_GATEWAY_TOKEN"
LOCAL_MODEL = "rebuilt-bps-smoke"
BODY_LIMIT = 512 * 1024
REQUEST_TIMEOUT = 45
EVENT_TYPES = {
    "response.created", "response.in_progress", "response.output_item.added",
    "response.content_part.added", "response.output_text.delta", "response.output_text.done",
    "response.content_part.done", "response.output_item.done", "response.completed",
    "response.incomplete", "response.failed", "response.error", "error",
}
TERMINAL_ERRORS = {"response.incomplete", "response.failed", "response.error", "error"}
ERROR_CODES = {
    "bps_credential_unavailable", "bps_credential_origin_mismatch", "bps_upstream_rejected",
    "bps_transport_failed_no_replay", "bps_stream_interrupted", "model_not_found",
    "bps_concurrency_policy_unavailable", "bps_concurrency_wait_timeout",
    "bps_plugin_unavailable", "bps_request_too_large", "bps_requires_full_history",
    "internal_server_error", "invalid_request_error", "invalid_api_key", "unknown_error",
    "insufficient_quota", "rate_limit_exceeded", "request_timeout", "context_length_exceeded",
    "unsupported_executor_route", "stream_truncated",
}
ERROR_TYPES = {"invalid_request_error", "server_error", "authentication_error", "rate_limit_error",
               "insufficient_quota", "permission_error", "overloaded_error"}
ERROR_MESSAGE_CLASSES = {
    "bps_stream_interrupted": "original_bps_stream_interruption",
    "the upstream did not return a complete Responses result": "rebuilt_incomplete_result",
    "upstream stream closed before a terminal event (last event: response.in_progress)": "cpa_missing_terminal",
}


class DenyProxy(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def deny(self):
        self.send_response(403)
        self.send_header("Content-Length", "0")
        self.end_headers()

    do_CONNECT = do_GET = deny


def endpoint(value):
    try:
        parsed = urlsplit(value)
        invalid = parsed.scheme != "https" or not parsed.hostname or parsed.username is not None
        invalid |= parsed.path != "/v1/responses" or "?" in value or "#" in value
        invalid |= any(ord(character) <= 32 or ord(character) >= 127 for character in value)
        invalid |= parsed.port == 0
    except ValueError:
        invalid = True
    if invalid:
        raise argparse.ArgumentTypeError("expected an HTTPS /v1/responses URL without userinfo, query or fragment")
    return parsed


def model_id(value):
    if not re.fullmatch(r"[A-Za-z0-9_.:/-]{1,128}", value):
        raise argparse.ArgumentTypeError("expected a gateway model identifier")
    return value


def reserve_port():
    with socket.socket() as sock:
        sock.bind((LOOPBACK, 0))
        return sock.getsockname()[1]


def wait_for_model(process, port, key):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline and process.poll() is None:
        try:
            with closing(http.client.HTTPConnection(LOOPBACK, port, timeout=1)) as client:
                client.request("GET", "/v1/models", headers={"Authorization": f"Bearer {key}"})
                with client.getresponse() as response:
                    if response.status == 200:
                        models = json.loads(response.read(BODY_LIMIT))["data"]
                        if any(model.get("id") == LOCAL_MODEL for model in models):
                            return True
        except (OSError, ValueError, TypeError, KeyError, AttributeError, http.client.HTTPException):
            pass
        time.sleep(0.15)
    return False


def elapsed_ms(start):
    return round((time.monotonic() - start) * 1000)


def classify_error(event):
    response = event.get("response")
    detail = response.get("error") if isinstance(response, dict) else None
    if not isinstance(detail, dict):
        detail = event.get("error")
    if not isinstance(detail, dict):
        detail = event
    summary = {}
    for field, allowed in (("code", ERROR_CODES), ("type", ERROR_TYPES)):
        value = detail.get(field)
        if isinstance(value, str) and value:
            summary[field] = value if value in allowed else "other"
    message = detail.get("message")
    if isinstance(message, str) and message in ERROR_MESSAGE_CLASSES:
        summary["classification"] = ERROR_MESSAGE_CLASSES[message]
    return summary


def measure_stream(response, result, start, marker):
    events = Counter()
    fragments = []
    frame = []
    bytes_read = 0
    finished = False
    try:
        while True:
            line = response.readline(BODY_LIMIT + 1)
            bytes_read += len(line)
            if bytes_read > BODY_LIMIT:
                result["failure"] = "event_limit"
                break
            if line.startswith(b"data:"):
                frame.append(line[5:].strip())
                continue
            if line.strip():
                continue
            if frame:
                data = b"\n".join(frame)
                frame.clear()
                if data != b"[DONE]":
                    event = json.loads(data)
                    kind = event.get("type")
                    events[kind if isinstance(kind, str) and kind in EVENT_TYPES else "other"] += 1
                    if kind == "response.output_text.delta" and isinstance(event.get("delta"), str):
                        result.setdefault("first_delta_ms", elapsed_ms(start))
                        fragments.append(event["delta"])
                    if kind == "response.completed":
                        finished = event.get("response", {}).get("status") == "completed"
                        result.setdefault("completed_ms", elapsed_ms(start))
                    if isinstance(kind, str) and kind in TERMINAL_ERRORS:
                        result.setdefault("terminal_error", {"event": kind, **classify_error(event)})
            if not line:
                break
    finally:
        result["event_counts"] = dict(events)
        result["marker_matched"] = "".join(fragments).strip() == marker
        result["completed"] = finished and events["response.completed"] == 1
    if "failure" not in result and result["completed"] and result["marker_matched"] and not TERMINAL_ERRORS.intersection(events):
        result["passed"] = True
    elif "failure" not in result:
        result["failure"] = "unexpected_stream_result"


def abort_socket(sock):
    try:
        sock.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass


def request_once(port, client_key, result):
    marker = "BPS" + secrets.token_hex(4).upper()
    body = json.dumps({
        "model": LOCAL_MODEL,
        "input": f"Reply with exactly this marker and no other text\n{marker}",
        "stream": True,
        "reasoning": {"effort": "low"},
        "store": False,
    }).encode()
    headers = {
        "Authorization": f"Bearer {client_key}",
        "Content-Type": "application/json",
        "Accept": "text/event-stream",
    }
    start = time.monotonic()
    try:
        with closing(http.client.HTTPConnection(LOOPBACK, port, timeout=REQUEST_TIMEOUT)) as client:
            client.connect()
            deadline = threading.Timer(REQUEST_TIMEOUT, abort_socket, args=(client.sock,))
            deadline.start()
            try:
                result["post_attempts"] = 1
                client.request("POST", "/v1/responses", body, headers)
                with client.getresponse() as response:
                    result["headers_ms"] = elapsed_ms(start)
                    result["http_status"] = response.status
                    content_type = response.headers.get_content_type()
                    result["sse"] = content_type == "text/event-stream"
                    if response.status != 200 or not result["sse"]:
                        result["failure"] = "unexpected_http_response"
                        if content_type == "application/json":
                            error_body = json.loads(response.read(4096))
                            if isinstance(error_body, dict):
                                summary = classify_error(error_body)
                                if summary:
                                    result["error"] = summary
                        return
                    measure_stream(response, result, start, marker)
            finally:
                deadline.cancel()
    except (OSError, ValueError, TypeError, AttributeError, http.client.HTTPException):
        result.setdefault("failure", "local_request_or_response_error")
    finally:
        result["duration_ms"] = elapsed_ms(start)


def run(args, token, result):
    with tempfile.TemporaryDirectory(prefix="cpa-bps-live-") as workdir:
        root = Path(workdir)
        auths, plugins = root / "auths", root / "plugins"
        auths.mkdir()
        plugins.mkdir()
        client_key = secrets.token_urlsafe()
        port = reserve_port()
        origin = f"{args.upstream_url.scheme}://{args.upstream_url.netloc}"
        (auths / "delegated.json").write_text(json.dumps({
            "type": "cpa-bps", "token_env": TOKEN_ENV, "upstream_origin": origin,
        }))
        shutil.copyfile(args.plugin, plugins / ("cpa-bps" + args.plugin.suffix))
        cfg = root / "config.yaml"
        cfg.write_text(json.dumps({
            "host": LOOPBACK, "port": port, "auth-dir": str(auths),
            "api-keys": [client_key],
            "remote-management": {"disable-control-panel": True, "disable-auto-update-panel": True},
            "routing": {"retry": {"request-retry": 0, "max-retry-credentials": 1, "max-retry-interval": 0}},
            "requests": {"streaming": {"bootstrap-retries": 0}},
            "plugins": {"enabled": True, "dir": str(plugins), "configs": {
                "cpa-bps": {"enabled": True, "responses_url": args.upstream_url.geturl(),
                            "models": [{"name": args.model, "alias": LOCAL_MODEL}],
                            "account_concurrency_limit": 1},
            }},
        }))
        with http.server.ThreadingHTTPServer((LOOPBACK, 0), DenyProxy) as proxy:
            proxy_url = f"http://{LOOPBACK}:{proxy.server_port}"
            blocked = threading.Thread(target=proxy.serve_forever, daemon=True)
            blocked.start()
            try:
                env = {
                    "XDG_CONFIG_HOME": workdir, "XDG_CACHE_HOME": workdir, "TMPDIR": workdir,
                    TOKEN_ENV: token, "HTTP_PROXY": proxy_url, "HTTPS_PROXY": proxy_url, "NO_PROXY": LOOPBACK,
                }
                process = subprocess.Popen(
                    [str(args.cpa), "-local-model", "-config", str(cfg)], cwd=root, env=env,
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                )
                try:
                    result["registered_model"] = wait_for_model(process, port, client_key)
                    if result["registered_model"]:
                        request_once(port, client_key, result)
                    else:
                        result["failure"] = "local_plugin_not_ready"
                        if process.returncode is not None:
                            result["local_exit_code"] = process.returncode
                finally:
                    if process.poll() is None:
                        try:
                            process.terminate()
                        except ProcessLookupError:
                            pass
                    try:
                        process.wait(timeout=4)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)
            finally:
                proxy.shutdown()
                blocked.join(timeout=2)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=Path, required=True)
    parser.add_argument("--plugin", type=Path, required=True)
    parser.add_argument("--upstream-url", type=endpoint, required=True)
    parser.add_argument("--model", type=model_id, required=True)
    parser.add_argument("--evidence", type=Path)
    args = parser.parse_args()
    token = os.environ.get(TOKEN_ENV, "")
    if not token or any(ord(character) <= 32 or ord(character) >= 127 for character in token):
        parser.error(f"{TOKEN_ENV} must contain the authorized gateway API key")
    args.cpa, args.plugin = args.cpa.resolve(), args.plugin.resolve()
    if not args.cpa.is_file() or not args.plugin.is_file() or args.plugin.suffix not in {".so", ".dylib"}:
        parser.error("expected a CPA executable and a .so or .dylib")
    try:
        evidence = args.evidence.open("x", encoding="utf-8") if args.evidence else nullcontext()
    except OSError:
        parser.error("unable to create a new evidence file")
    result = {
        "started_at": datetime.now(timezone(timedelta(hours=8))).isoformat(timespec="seconds"),
        "route": ["loopback-cpa", "rebuilt-plugin", "authorized-responses-gateway"],
        "upstream_model": args.model, "post_attempts": 0, "passed": False,
    }
    with evidence as report:
        try:
            run(args, token, result)
        except (OSError, subprocess.SubprocessError):
            result.setdefault("failure", "local_setup_or_cleanup_error")
            result["passed"] = False
        except KeyboardInterrupt:
            result.setdefault("failure", "interrupted")
            result["passed"] = False
        output = json.dumps(result, ensure_ascii=False, indent=2) + "\n"
        if report is not None:
            report.write(output)
        print(output, end="")
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
