#!/usr/bin/env python3
"""Compare real cache usage without changing gateway accounts or configuration."""

import argparse
import copy
from datetime import datetime, timezone
import hashlib
import http.client
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import signal
from urllib.parse import urlsplit

from cache_acceptance import RUNBOOK, QUESTIONS, TOOLS, GROUPS, cache_usage, aggregate
from real_acceptance import Runner, model_id


API_ENV = "CPA_BPS_TEST_API_KEY"
REQUEST_BUDGET = 12
INSTRUCTION_PREFIX = (
    "Use the following synthetic release handbook to answer the user's questions. "
    "All requested answers are present in this document. "
    "Return only the requested answer and do not call a tool."
)


def comparison_url(value):
    """Allow cleartext only on numeric loopback; do not follow redirects."""
    try:
        parsed = urlsplit(value)
        if not parsed.hostname or parsed.scheme not in {"http", "https"}:
            raise ValueError
        try:
            loopback = ipaddress.ip_address(parsed.hostname).is_loopback
        except ValueError:
            loopback = False
        if parsed.scheme == "http" and not loopback:
            raise ValueError
        if (parsed.username is not None or parsed.password is not None
                or parsed.path not in {"", "/", "/v1", "/v1/"} or parsed.query or parsed.fragment
                or parsed.port == 0 or any(ord(char) <= 32 or ord(char) >= 127 for char in value)):
            raise ValueError
    except (ValueError, TypeError):
        raise argparse.ArgumentTypeError("base URL must be numeric-loopback HTTP or HTTPS, optionally ending in /v1")
    return parsed


def label_value(value):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}", value):
        raise argparse.ArgumentTypeError("label must contain 1..64 ASCII letters, digits, dots, underscores or hyphens")
    return value


def groups_value(value):
    result = tuple(value.split(","))
    if not result or any(group not in GROUPS for group in result) or len(set(result)) != len(result):
        raise argparse.ArgumentTypeError("groups must be distinct comma-separated multi_turn,exact_repeat,catalog_repeat")
    return result


def fixture_fingerprint(groups, probe_only):
    fixture = {
        "instructions": INSTRUCTION_PREFIX, "runbook": RUNBOOK,
        "questions": QUESTIONS, "tools": TOOLS, "groups": groups, "probe_only": probe_only,
        "session_fields": ["X-Session-ID", "prompt_cache_key"],
        "session_marker_format": "BPS_COMPARE_ plus 32 uppercase hex characters",
    }
    return hashlib.sha256(json.dumps(fixture, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def terminal_metadata(response):
    """Retain comparison fields without response text or raw session identifiers."""
    response = response if isinstance(response, dict) else {}
    metadata = response.get("metadata")
    metadata = metadata if isinstance(metadata, dict) else {}

    def safe_name(value, maximum):
        return value if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.:/-]{1," + str(maximum) + r"}", value) else None

    def digest(value):
        return hashlib.sha256(value.encode()).hexdigest() if isinstance(value, str) and value else None

    instructions, tools = response.get("instructions"), response.get("tools")
    identifiers = ("prompt_cache_key", "task_id", "turn_id")
    return {
        "model": safe_name(response.get("model"), 128),
        "prompt_cache_retention": safe_name(response.get("prompt_cache_retention"), 64),
        "instructions_bytes": len(instructions.encode()) if isinstance(instructions, str) else None,
        "native_tools_count": len(tools) if isinstance(tools, list) else None,
        "identifiers_sha256": {name: digest(response.get(name)) for name in identifiers},
        "metadata_identifiers_sha256": {name: digest(metadata.get(name)) for name in identifiers},
    }


class ComparisonSuite:
    def __init__(self, args, runner):
        self.args, self.runner = args, runner
        self.reports, self.cases = [], []
        self.stopped_after = None

    def call(self, group, index, messages, expected, instructions, session, **options):
        if len(self.reports) >= (1 if self.args.probe_only else REQUEST_BUDGET):
            raise RuntimeError("comparison request budget exhausted")
        report = {
            "request": group + "_" + str(index + 1), "group": group, "position": index + 1,
            "cache_stage": "first_observation" if index == 0 else "warm_candidate",
            "attempts": 1, "completed": False, "passed": False,
            "cache_usage": cache_usage(None), "usage_raw": None,
            "input_bytes": len(json.dumps(messages, ensure_ascii=False).encode()),
            "instructions_bytes": len(instructions.encode()),
        }
        # Append before I/O so an interruption or raised error remains in evidence.
        self.reports.append(report)
        response, text = None, ""
        try:
            observed, response, text = self.runner.call(
                report["request"], messages, stream=True, session_header=session,
                prompt_cache_key=session, instructions=instructions, **options,
            )
            report.update(observed)
        except KeyboardInterrupt:
            report["failure"] = "interrupted"
            raise
        except (OSError, ValueError, TypeError, RuntimeError, http.client.HTTPException):
            report["failure"] = "request_exception"
        finally:
            report["cache_usage"] = cache_usage(response)
            usage = response.get("usage") if isinstance(response, dict) else None
            report["usage_raw"] = copy.deepcopy(usage) if isinstance(usage, dict) else None
            report["terminal_metadata"] = terminal_metadata(response)
        report["expected_answer_matched"] = text.strip() == expected
        report["passed"] = bool(
            report.get("completed") is True and report.get("text_consistent") is True
            and report["expected_answer_matched"] and not report.get("failure")
            and isinstance(response, dict) and isinstance(response.get("output"), list)
        )
        if not report["passed"]:
            self.stopped_after = report["request"]
            report["stop_reason"] = ("http_" + str(report["http"]) if report.get("http") in (403, 429)
                                     else report.get("failure", "response_validation_failed"))
        print(json.dumps({
            "request": report["request"], "passed": report["passed"], "http": report.get("http"),
            "cache_usage": report["cache_usage"],
        }), flush=True)
        return report, response

    def run_group(self, group):
        session = "BPS_COMPARE_" + secrets.token_hex(16).upper()
        instructions = "Session " + session + ". " + INSTRUCTION_PREFIX + "\n\n" + RUNBOOK
        count = 6 if group == "multi_turn" else 3
        if group == "probe":
            count, instructions = 1, "Return only the exact requested text."
        case = {"case": group, "expected_requests": count, "passed": False, "requests": []}
        self.cases.append(case)
        history = []
        for index in range(count):
            question, expected = QUESTIONS[index] if group == "multi_turn" else QUESTIONS[0]
            if group == "probe":
                question, expected = "Reply only READY.", "READY"
            elif index == 0 or group != "multi_turn":
                question = "Benchmark session " + session + ". " + question
            current = history + [{"role": "user", "content": question}] if group == "multi_turn" else [
                {"role": "user", "content": question}]
            options = {}
            if group == "catalog_repeat":
                rotation = index % len(TOOLS)
                options = {"tools": TOOLS[rotation:] + TOOLS[:rotation], "tool_choice": "auto"}
            before = len(self.reports)
            try:
                report, response = self.call(group, index, current, expected, instructions, session, **options)
            finally:
                # Keep the failed or interrupted attempt in its group as well.
                case["requests"].extend(self.reports[before:])
            if not report["passed"]:
                break
            if group == "multi_turn":
                history = current + response["output"]
        case["passed"] = len(case["requests"]) == count and all(row["passed"] for row in case["requests"])
        return case["passed"]

    def run(self):
        for group in (("probe",) if self.args.probe_only else self.args.groups):
            if not self.run_group(group):
                return False
        return True


def interrupted(*_):
    raise KeyboardInterrupt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", type=comparison_url, required=True)
    parser.add_argument("--model", type=model_id, default="gpt-6.2-sol")
    parser.add_argument("--label", type=label_value, required=True)
    parser.add_argument("--evidence", type=Path, required=True, help="parent directory for a new evidence run")
    parser.add_argument("--groups", type=groups_value, default=GROUPS)
    parser.add_argument("--probe-only", action="store_true", help="send only one short READY request")
    parser.add_argument("--request-timeout", type=int, default=60, choices=range(15, 121), metavar="15..120")
    args = parser.parse_args()
    key = os.environ.get(API_ENV, "")
    if not key or any(ord(char) <= 32 or ord(char) >= 127 for char in key):
        parser.error("CPA_BPS_TEST_API_KEY must contain a printable ASCII client key")
    args.evidence.mkdir(parents=True, exist_ok=True)
    now = datetime.now(timezone.utc)
    directory = args.evidence / (now.strftime("%Y%m%dT%H%M%SZ") + "-" + args.label + "-" + secrets.token_hex(3))
    directory.mkdir(mode=0o700)
    suite = ComparisonSuite(args, Runner(args, key))
    summary = {
        "label": args.label, "base_url": args.base_url.geturl(), "model": args.model,
        "started_at": now.isoformat(timespec="seconds"), "probe_only": args.probe_only,
        "groups": ["probe"] if args.probe_only else list(args.groups),
        "request_budget": 1 if args.probe_only else sum(6 if group == "multi_turn" else 3 for group in args.groups),
        "request_timeout_seconds": args.request_timeout,
        "fixture_sha256": hashlib.sha256(RUNBOOK.encode()).hexdigest(),
        "protocol_sha256": fixture_fingerprint(args.groups, args.probe_only),
        "fixture_words": len(RUNBOOK.split()), "fixture_bytes": len(RUNBOOK.encode()),
        "session_fields": ["X-Session-ID", "prompt_cache_key"],
        "account_selection": "existing gateway routing; no account attribution or configuration changes",
        "measurement": "upstream usage.input_tokens_details.cached_tokens / usage.input_tokens",
        "first_observation": "Fresh unique session markers; server-side cache coldness is not assumed.",
        "cache_write_measurement": "Reported write counter and TTL buckets are retained separately, without inference.",
        "passed": False, "evidence_dir": str(directory),
    }
    previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        summary["passed"] = suite.run()
    except KeyboardInterrupt:
        summary["failure"] = "interrupted"
    except (OSError, ValueError, TypeError, RuntimeError, http.client.HTTPException):
        summary["failure"] = "comparison_failed"
    finally:
        for sig in previous:
            signal.signal(sig, signal.SIG_IGN)
        summary["generation_requests"] = len(suite.reports)
        summary["stopped_after"] = suite.stopped_after
        summary["cases"] = suite.cases
        for case in suite.cases:
            case["all"] = aggregate(case["requests"])
            case["first"] = aggregate(case["requests"][:1])
            case["warm"] = aggregate(case["requests"][1:])
        summary["all"] = aggregate(suite.reports)
        summary["warm"] = aggregate([row for row in suite.reports if row["cache_stage"] == "warm_candidate"])
        summary["finished_at"] = datetime.now(timezone.utc).isoformat(timespec="seconds")
        if summary.get("failure"):
            summary["passed"] = False
        with (directory / "summary.json").open("x", encoding="utf-8") as file:
            json.dump(summary, file, ensure_ascii=False, indent=2)
            file.write("\n")
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    print(json.dumps({
        "label": args.label, "passed": summary["passed"], "generation_requests": len(suite.reports),
        "stopped_after": suite.stopped_after, "all": summary["all"], "warm": summary["warm"],
        "evidence_dir": str(directory),
    }), flush=True)
    return 0 if summary["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

