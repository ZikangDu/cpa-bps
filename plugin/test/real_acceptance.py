#!/usr/bin/env python3
"""Run bounded, synthetic acceptance requests against an existing local CPA."""

import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from datetime import datetime, timedelta, timezone
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import socket
import threading
import time
from urllib.parse import urlsplit

from live_responses import ERROR_CODES, EVENT_TYPES, TERMINAL_ERRORS, classify_error


KEY_ENV = "CPA_BPS_TEST_API_KEY"
REQUEST_SECONDS = 45
MAX_RESPONSE_BYTES = 2 << 20
MAX_FRAME_BYTES = 512 << 10
CASES = ("text", "sse", "history", "function", "custom", "json_schema", "image", "concurrency")
DEFAULT_CASES = CASES[:-1]
RED_PNG = (
    "iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAAb0lEQVR4nO3PAQkAAAyEwO9feoshgnABdLep"
    "8QUNyPEFDcjxBQ3I8QUNyPEFDcjxBQ3I8QUNyPEFDcjxBQ3I8QUNyPEFDcjxBQ3I8QUNyPEFDcjxBQ3I"
    "8QUNyPEFDcjxBQ3I8QUNyPEFDcjxBQ3IPanc8OLDQitxAAAAAElFTkSuQmCC"
)
KNOWN_EVENTS = EVENT_TYPES | {
    "response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
    "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
    "response.reasoning_text.delta", "response.reasoning_text.done",
    "response.function_call_arguments.delta", "response.function_call_arguments.done",
    "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done",
}
KNOWN_CODES = ERROR_CODES | {
    "bps_oauth_expired", "bps_oauth_client_mismatch", "bps_oauth_reconnect_required",
    "bps_invalid_tool_response", "bps_invalid_tool_catalog", "bps_invalid_history",
    "bps_invalid_inline_image", "bps_inline_image_limit", "bps_image_upload_failed",
    "invalid_tool_call", "missing_tool_call", "unsupported_input_item",
}
TERMINALS = {"response.completed", "response.incomplete", "response.failed"}


def base_url(value):
    try:
        parsed = urlsplit(value)
        invalid = parsed.scheme not in {"http", "https"} or parsed.hostname not in {"127.0.0.1", "::1"}
        invalid |= parsed.username is not None or parsed.password is not None
        invalid |= parsed.path not in {"", "/", "/v1", "/v1/"} or bool(parsed.query or parsed.fragment)
        invalid |= parsed.port == 0 or any(ord(char) <= 32 for char in value)
    except ValueError:
        invalid = True
    if invalid:
        raise argparse.ArgumentTypeError("base URL must be an HTTP(S) numeric loopback origin, optionally ending in /v1")
    return parsed


def model_id(value):
    if not re.fullmatch(r"[A-Za-z0-9_.:/-]{1,128}", value):
        raise argparse.ArgumentTypeError("invalid model identifier")
    return value


def marker():
    return "BPS_TEST_" + secrets.token_hex(4).upper()


def clock_ms(start):
    return round((time.monotonic() - start) * 1000)


def response_text(response):
    if not isinstance(response, dict):
        return ""
    parts = []
    for item in response.get("output", []):
        if not isinstance(item, dict) or item.get("type") != "message":
            continue
        for part in item.get("content", []):
            if isinstance(part, dict) and part.get("type") == "output_text" and isinstance(part.get("text"), str):
                parts.append(part["text"])
    return "".join(parts)


def safe_error(event):
    if not isinstance(event, dict):
        return {}
    summary = classify_error(event)
    response = event.get("response")
    detail = response.get("error") if isinstance(response, dict) else None
    if not isinstance(detail, dict):
        detail = event.get("error")
    if not isinstance(detail, dict):
        detail = event
    code = detail.get("code")
    if isinstance(code, str) and code in KNOWN_CODES:
        summary["code"] = code
    message = detail.get("message")
    if code == "invalid_tool_call" and isinstance(message, str):
        reason = message.removeprefix("Basis Points returned an invalid client tool relay: ")
        if re.fullmatch(r"[a-z_ ]{1,100}", reason):
            summary["reason"] = reason
    return summary


def safe_usage(response):
    usage = response.get("usage") if isinstance(response, dict) else None
    if not isinstance(usage, dict):
        return {}
    result = {key: usage[key] for key in ("input_tokens", "output_tokens", "total_tokens")
              if isinstance(usage.get(key), int) and not isinstance(usage[key], bool) and usage[key] >= 0}
    for source, field in (("input_tokens_details", "cached_tokens"), ("output_tokens_details", "reasoning_tokens")):
        detail = usage.get(source)
        value = detail.get(field) if isinstance(detail, dict) else None
        if isinstance(value, int) and not isinstance(value, bool) and value >= 0:
            result[field] = value
    return result


def abort(sock):
    try:
        sock.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass


def read_sse(reply, evidence, start):
    frame = []
    frame_bytes = 0
    total_bytes = 0
    name = ""
    counts = Counter()
    fragments = []
    response = None
    terminal_count = 0
    errors = False

    def consume():
        nonlocal response, terminal_count, errors
        if not frame:
            return
        payload = b"\n".join(frame)
        if payload.strip() == b"[DONE]":
            return
        event = json.loads(payload)
        if not isinstance(event, dict):
            raise ValueError("invalid SSE object")
        kind = event.get("type") or name
        counts[kind if isinstance(kind, str) and kind in KNOWN_EVENTS else "other"] += 1
        if kind == "response.output_text.delta" and isinstance(event.get("delta"), str):
            fragments.append(event["delta"])
            evidence.setdefault("first_text_ms", clock_ms(start))
        if isinstance(kind, str) and kind in TERMINALS:
            terminal_count += 1
            response = event.get("response")
            evidence.setdefault("terminal_ms", clock_ms(start))
        if isinstance(kind, str) and kind in TERMINAL_ERRORS:
            errors = True
            evidence.setdefault("terminal_error", {"event": kind, **safe_error(event)})

    try:
        while True:
            line = reply.readline(MAX_FRAME_BYTES + 1)
            total_bytes += len(line)
            if len(line) > MAX_FRAME_BYTES or total_bytes > MAX_RESPONSE_BYTES:
                evidence["failure"] = "response_limit"
                break
            stripped = line.rstrip(b"\r\n")
            if not stripped:
                consume()
                frame.clear()
                frame_bytes = 0
                name = ""
                if not line:
                    break
                continue
            if stripped.startswith(b"event:"):
                name = stripped[6:].strip().decode("utf-8")
            elif stripped.startswith(b"data:"):
                data = stripped[5:].removeprefix(b" ")
                frame_bytes += len(data)
                if frame_bytes > MAX_FRAME_BYTES:
                    evidence["failure"] = "event_limit"
                    break
                frame.append(data)
    finally:
        evidence["events"] = dict(counts)
        evidence["terminal_count"] = terminal_count
    text = "".join(fragments)
    if not evidence.get("failure") and terminal_count == 1 and not errors and isinstance(response, dict) and response.get("status") == "completed":
        evidence["completed"] = True
    else:
        evidence.setdefault("failure", "unexpected_stream_terminal")
    evidence["text_consistent"] = text == response_text(response)
    return response, text


class Runner:
    def __init__(self, args, key):
        self.args = args
        self.key = key
        self.request_seconds = getattr(args, "request_timeout", REQUEST_SECONDS)

    def call(self, name, input_value, *, stream=False, session_header=None, **options):
        if session_header is not None and (
            not isinstance(session_header, str)
            or not re.fullmatch(r"[\x20-\x7e]{1,256}", session_header)
        ):
            raise ValueError("session_header must contain 1..256 printable ASCII characters")
        payload = {"model": self.args.model, "input": input_value, "stream": stream,
                   "store": False, "reasoning": {"effort": "low"}, **options}
        headers = {
            "Authorization": "Bearer " + self.key,
            "Content-Type": "application/json",
            "Accept": "text/event-stream" if stream else "application/json",
        }
        if session_header is not None:
            headers["X-Session-ID"] = session_header
        evidence = {"request": name, "stream": stream, "attempts": 1, "completed": False,
                    "request_timeout_seconds": self.request_seconds}
        cls = http.client.HTTPSConnection if self.args.base_url.scheme == "https" else http.client.HTTPConnection
        start = time.monotonic()
        response = None
        text = ""
        timed_out = threading.Event()
        with closing(cls(self.args.base_url.hostname, self.args.base_url.port, timeout=5)) as conn:
            try:
                conn.connect()
                remaining = max(.1, self.request_seconds - (time.monotonic() - start))
                conn.sock.settimeout(remaining)

                def deadline():
                    timed_out.set()
                    abort(socket_ref)

                socket_ref = conn.sock
                timer = threading.Timer(remaining, deadline)
                timer.start()
                try:
                    conn.request("POST", "/v1/responses", json.dumps(payload).encode(), headers)
                    with conn.getresponse() as reply:
                        evidence["http"] = reply.status
                        evidence["headers_ms"] = clock_ms(start)
                        content_type = reply.headers.get_content_type()
                        evidence["content_type"] = content_type if content_type in {"application/json", "text/event-stream", "text/html"} else "other"
                        if reply.status == 200 and content_type == "text/event-stream":
                            response, text = read_sse(reply, evidence, start)
                            if not stream:
                                evidence["failure"] = "unexpected_sse_for_nonstream"
                        else:
                            data = reply.read(MAX_RESPONSE_BYTES + 1)
                            if len(data) > MAX_RESPONSE_BYTES:
                                evidence["failure"] = "response_limit"
                            elif content_type == "application/json":
                                parsed = json.loads(data)
                                if reply.status == 200 and isinstance(parsed, dict) and not stream:
                                    response = parsed
                                    text = response_text(response)
                                    evidence["completed"] = response.get("status") == "completed"
                                    if not evidence["completed"]:
                                        evidence["failure"] = "unexpected_response_status"
                                        evidence["error"] = safe_error(response)
                                else:
                                    evidence["failure"] = "unexpected_http_response"
                                    evidence["error"] = safe_error(parsed)
                            else:
                                evidence["failure"] = "unexpected_http_response"
                finally:
                    timer.cancel()
            except (OSError, ValueError, TypeError, AttributeError, http.client.HTTPException):
                evidence.setdefault("failure", "request_or_response_error")
        if timed_out.is_set():
            evidence["failure"] = "request_deadline"
            evidence["completed"] = False
        if response is not None and (not isinstance(response, dict) or not isinstance(response.get("output"), list)):
            evidence["failure"] = "invalid_response_shape"
            evidence["completed"] = False
            response = None
        evidence["duration_ms"] = clock_ms(start)
        if response is not None:
            evidence["usage"] = safe_usage(response)
        return evidence, response, text

    def exact(self, name, expected, *, stream=False, input_value=None, **options):
        prompt = input_value if input_value is not None else "Reply with exactly " + expected + " and nothing else."
        evidence, response, text = self.call(name, prompt, stream=stream, **options)
        evidence["marker_matched"] = text.strip() == expected
        evidence["passed"] = (evidence["completed"] and evidence["marker_matched"] and not evidence.get("failure")
                              and (not stream or evidence.get("text_consistent") is True))
        return evidence, response

    def run_case(self, name):
        if name in {"text", "sse"}:
            expected = marker()
            evidence, _ = self.exact(name, expected, stream=name == "sse")
            return {"case": name, "expected_marker": expected, "passed": evidence["passed"], "requests": [evidence]}
        return getattr(self, name)()

    def history(self):
        expected = marker()
        first = "Remember this exact code for our conversation: " + expected + ". Reply only READY."
        opening, response = self.exact("history_open", "READY", input_value=first)
        reports = [opening]
        if opening["passed"]:
            history = [{"role": "user", "content": first}] + response.get("output", [])
            history.append({"role": "user", "content": "Return only the exact code I asked you to remember."})
            answer, _ = self.exact("history_recall", expected, stream=True, input_value=history)
            reports.append(answer)
        return {"case": "history", "expected_marker": expected,
                "passed": len(reports) == 2 and all(item["passed"] for item in reports), "requests": reports}

    def function(self):
        expected = marker()
        tool = {"type": "function", "name": "fixture_lookup", "description": "A synthetic acceptance test lookup.",
                "parameters": {"type": "object", "properties": {"value": {"type": "string"}},
                               "required": ["value"], "additionalProperties": False}, "strict": True}
        prompt = "Call fixture_lookup exactly once with value " + expected + ". Wait for its result."
        evidence, response, _ = self.call("function_call", prompt, stream=True, tools=[tool],
                                         tool_choice={"type": "function", "name": "fixture_lookup"})
        calls = [item for item in (response or {}).get("output", [])
                 if isinstance(item, dict) and item.get("type") in {"function_call", "custom_tool_call"}]
        call = calls[0] if len(calls) == 1 else {}
        try:
            args = json.loads(call.get("arguments", ""))
        except (ValueError, TypeError):
            args = None
        valid = (call.get("type") == "function_call" and call.get("name") == "fixture_lookup"
                 and args == {"value": expected} and isinstance(call.get("call_id"), str) and bool(call["call_id"]))
        evidence["tool_contract_matched"] = valid
        evidence["passed"] = evidence["completed"] and valid and not evidence.get("failure")
        reports = [evidence]
        if evidence["passed"]:
            result_marker = marker()
            history = [{"role": "user", "content": prompt}] + response["output"]
            history += [{"type": "function_call_output", "call_id": call["call_id"], "output": result_marker},
                        {"role": "user", "content": "Reply only with the exact tool result. Do not call a tool again."}]
            followup, _ = self.exact("function_result", result_marker, stream=True, input_value=history,
                                     tools=[tool], tool_choice="none")
            reports.append(followup)
        return {"case": "function", "expected_marker": expected, "tool_execution": "synthetic_result_only",
                "passed": len(reports) == 2 and all(item["passed"] for item in reports), "requests": reports}

    def custom(self):
        expected = marker()
        tool = {"type": "custom", "name": "fixture_echo", "description": "Accept one plain-text test marker.",
                "format": {"type": "text"}}
        evidence, response, _ = self.call("custom", "Call fixture_echo exactly once with the raw input " + expected + ".",
                                          stream=True, tools=[tool], tool_choice={"type": "custom", "name": "fixture_echo"})
        calls = [item for item in (response or {}).get("output", []) if isinstance(item, dict)
                 and item.get("type") in {"function_call", "custom_tool_call"}]
        call = calls[0] if len(calls) == 1 else {}
        valid = call.get("type") == "custom_tool_call" and call.get("name") == "fixture_echo" and call.get("input") == expected
        evidence["tool_contract_matched"] = valid
        evidence["passed"] = evidence["completed"] and valid and not evidence.get("failure")
        return {"case": "custom", "expected_marker": expected, "tool_execution": "none",
                "passed": evidence["passed"], "requests": [evidence]}

    def json_schema(self):
        expected = marker()
        format_spec = {"type": "json_schema", "name": "acceptance_marker", "strict": True,
                       "schema": {"type": "object", "properties": {"marker": {"type": "string"}},
                                  "required": ["marker"], "additionalProperties": False}}
        evidence, _, text = self.call("json_schema", "Return JSON with marker equal to " + expected + ".",
                                      text={"format": format_spec})
        try:
            valid = json.loads(text) == {"marker": expected}
        except ValueError:
            valid = False
        evidence["schema_matched"] = valid
        evidence["passed"] = evidence["completed"] and valid and not evidence.get("failure")
        return {"case": "json_schema", "expected_marker": expected, "passed": evidence["passed"], "requests": [evidence]}

    def image(self):
        content = [{"type": "input_text", "text": "What single primary color fills this image? Reply with only one lowercase English color word."},
                   {"type": "input_image", "image_url": "data:image/png;base64," + RED_PNG, "detail": "low"}]
        evidence, _ = self.exact("image", "red", stream=True, input_value=[{"role": "user", "content": content}])
        return {"case": "image", "fixture": "64x64_rgb_red_png", "passed": evidence["passed"], "requests": [evidence]}

    def concurrency(self):
        barrier = threading.Barrier(2)

        def request(index):
            expected = marker()
            barrier.wait(timeout=5)
            evidence, _ = self.exact("concurrency_" + str(index), expected, stream=True)
            return evidence

        with ThreadPoolExecutor(max_workers=2) as pool:
            reports = list(pool.map(request, (1, 2)))
        return {"case": "concurrency", "simultaneous_clients": 2,
                "passed": all(item["passed"] for item in reports), "requests": reports}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", type=base_url, required=True)
    parser.add_argument("--model", type=model_id, default="gpt-6.2-sol")
    parser.add_argument("--evidence", type=Path, required=True, help="parent directory for new per-run evidence")
    parser.add_argument("--case", action="append", choices=CASES, help="run only selected case(s), in argument order")
    args = parser.parse_args()
    key = os.environ.get(KEY_ENV, "")
    if not key or any(ord(character) <= 32 or ord(character) >= 127 for character in key):
        parser.error(KEY_ENV + " must contain the existing local CPA client API key")
    args.evidence.mkdir(parents=True, exist_ok=True)
    now = datetime.now(timezone(timedelta(hours=8)))
    run_name = now.strftime("%Y%m%dT%H%M%S") + "-" + secrets.token_hex(3)
    directory = args.evidence / run_name
    directory.mkdir(mode=0o700)
    runner = Runner(args, key)
    chosen = args.case or DEFAULT_CASES
    summaries = []
    stopped = False
    for name in chosen:
        result = runner.run_case(name)
        summaries.append({"case": name, "passed": result["passed"], "request_count": len(result["requests"])})
        with (directory / (f"{len(summaries):02d}_" + name + ".json")).open("x", encoding="utf-8") as file:
            json.dump(result, file, ensure_ascii=False, indent=2)
            file.write("\n")
        print(json.dumps(summaries[-1]), flush=True)
        if not args.case and name in {"text", "sse"} and not result["passed"]:
            stopped = True
            break
    summary = {"started_at": now.isoformat(timespec="seconds"), "base_url": args.base_url.geturl(),
               "model": args.model, "cases": summaries, "stopped_on_prerequisite": stopped,
               "passed": not stopped and all(result["passed"] for result in summaries), "evidence_dir": str(directory)}
    with (directory / "summary.json").open("x", encoding="utf-8") as file:
        json.dump(summary, file, ensure_ascii=False, indent=2)
        file.write("\n")
    print(json.dumps(summary, ensure_ascii=False))
    return 0 if summary["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
