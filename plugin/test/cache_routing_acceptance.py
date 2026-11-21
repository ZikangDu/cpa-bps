#!/usr/bin/env python3
"""Verify real session affinity and failover on an isolated two-account CPA."""

import argparse
from datetime import datetime, timedelta, timezone
import http.client
import json
import os
from pathlib import Path
import secrets
import signal
import threading

from real_acceptance import Runner, base_url, marker, model_id
from two_account_acceptance import AccountState, Admin, json_write, require


API_ENV = "CPA_BPS_TEST_API_KEY"
ADMIN_ENV = "CPA_BPS_TEST_ADMIN_KEY"
MAX_REQUESTS = 8


def success_count(record):
    value = record.get("success")
    return value if isinstance(value, int) and not isinstance(value, bool) and value >= 0 else None


def attribute_request(peaks, before, after):
    """Use only local counter observations, preserving ambiguous attribution."""
    active = sorted(label for label, count in peaks.items() if count > 0)
    deltas = {}
    for label in before:
        start, end = success_count(before[label]), success_count(after[label])
        deltas[label] = end - start if start is not None and end is not None and end >= start else None
    completed = sorted(label for label, count in deltas.items() if count is not None and count > 0)
    one_completion = len(completed) == 1 and deltas[completed[0]] == 1
    if one_completion and all(value in (0, 1) for value in deltas.values()):
        if not active or active == completed:
            return completed[0], "success_counter_and_active" if active else "success_counter", active, deltas
    if not completed and len(active) == 1:
        return active[0], "active_counter_only", active, deltas
    return None, "ambiguous_or_unobserved", active, deltas


class RoutingSuite:
    def __init__(self, args, runner, state):
        self.args, self.runner, self.state = args, runner, state
        self.reports = []

    @staticmethod
    def session(label):
        return {"label": label, "key": "BPS_AFFINITY_" + secrets.token_hex(16), "history": []}

    def ask(self, session, name, prompt, expected):
        require(len(self.reports) < MAX_REQUESTS, "routing benchmark request budget exhausted")
        require(self.state.drain(seconds=self.args.request_timeout), "previous local request did not drain")
        before = self.state.current()
        peaks = {label: 0 for label in self.state.accounts}
        stop = threading.Event()
        started = threading.Event()
        poll_errors = 0

        def observe():
            nonlocal poll_errors
            started.set()
            while not stop.is_set():
                try:
                    for label, snapshot in self.state.queues().items():
                        peaks[label] = max(peaks[label], snapshot["active"])
                except (OSError, ValueError, RuntimeError, http.client.HTTPException):
                    poll_errors += 1
                stop.wait(0.02)

        observer = threading.Thread(target=observe, daemon=True)
        observer.start()
        started.wait(timeout=2)
        history = session["history"] + [{"role": "user", "content": prompt}]
        session_option = "session_header" if self.args.session_field == "header" else "prompt_cache_key"
        try:
            evidence, response, text = self.runner.call(
                name, history, stream=True, **{session_option: session["key"]},
            )
        finally:
            stop.set()
            observer.join(timeout=18)
        require(not observer.is_alive(), "local queue observer did not finish")
        drained = self.state.drain(seconds=self.args.request_timeout)
        after = self.state.current()
        account, basis, active, deltas = attribute_request(peaks, before, after)
        evidence.update({
            "session": session["label"],
            "session_field": self.args.session_field,
            "marker_matched": text.strip() == expected,
            "attributed_account": account,
            "attribution_basis": basis,
            "observed_active_accounts": active,
            "success_counter_deltas": deltas,
            "peak_active_by_account": peaks,
            "counter_poll_errors": poll_errors,
            "drained": drained,
        })
        evidence["passed"] = bool(
            evidence.get("completed") and evidence["marker_matched"]
            and evidence.get("text_consistent") is True and not evidence.get("failure")
            and account is not None and drained
        )
        self.reports.append(evidence)
        if evidence["passed"]:
            session["history"] = history + response["output"]
        print(json.dumps({
            "request": name, "session": session["label"], "passed": evidence["passed"],
            "account": account, "attribution_basis": basis,
        }), flush=True)
        require(evidence["passed"], "a routing request or its local account attribution failed")
        return account

    def run(self):
        self.state.select(self.state.accounts)
        first = self.session("session_1")
        first_marker = marker()
        owner = self.ask(
            first, "initial_binding",
            "Remember this exact conversation code: " + first_marker + ". Reply only READY.", "READY",
        )
        second = self.ask(first, "same_session_second", "Reply only with the exact conversation code.", first_marker)
        third = self.ask(first, "same_session_third", "Repeat only the exact conversation code once more.", first_marker)
        stable = owner == second == third
        require(stable, "the same explicit session changed its assigned account")

        another = self.session("session_2")
        second_marker = marker()
        another_owner = self.ask(
            another, "new_session_binding",
            "Remember this exact conversation code: " + second_marker + ". Reply only READY.", "READY",
        )
        another_repeat = self.ask(
            another, "new_session_repeat", "Reply only with the exact conversation code.", second_marker,
        )
        require(another_owner == another_repeat, "the second session changed its assigned account")

        third_session = self.session("session_3")
        third_owner = self.ask(
            third_session, "third_session_binding",
            "This is a new independent conversation. Reply only READY.", "READY",
        )
        distributed = len({owner, another_owner, third_owner}) == 2
        require(distributed, "three fresh sessions did not use both available accounts")

        remaining = next(label for label in self.state.accounts if label != owner)
        self.state.select([remaining])
        failover_owner = self.ask(
            first, "bound_account_disabled",
            "Reply only with the original conversation code from this session.", first_marker,
        )
        require(failover_owner == remaining, "the session did not use the remaining enabled account")

        self.state.select(self.state.accounts)
        rebound_owner = self.ask(
            first, "failover_binding_retained",
            "Reply only with the original conversation code again.", first_marker,
        )
        rebound = rebound_owner == failover_owner
        return {
            "same_session_account_stable": stable,
            "second_session_account_stable": another_owner == another_repeat,
            "fresh_sessions_use_both_accounts": distributed,
            "disabled_binding_failed_over": failover_owner == remaining,
            "full_context_preserved_after_switch": self.reports[-2]["marker_matched"],
            "rebound_account_retained": rebound,
            "passed": stable and distributed and failover_owner == remaining and rebound,
        }


def interrupted(_signal, _frame):
    raise KeyboardInterrupt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", type=base_url, required=True)
    parser.add_argument("--model", type=model_id, default="gpt-6.2-sol")
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--session-field", choices=("prompt_cache_key", "header"), default="prompt_cache_key",
                        help="send the session through body prompt_cache_key (default) or X-Session-ID header")
    parser.add_argument("--request-timeout", type=int, default=60, choices=range(15, 121), metavar="15..120")
    args = parser.parse_args()
    keys = [os.environ.get(name, "") for name in (API_ENV, ADMIN_ENV)]
    if any(not key or any(ord(char) <= 32 or ord(char) >= 127 for char in key) for key in keys):
        parser.error("the local client and management key environment variables are required")

    args.evidence.mkdir(parents=True, exist_ok=True)
    now = datetime.now(timezone(timedelta(hours=8)))
    directory = args.evidence / (now.strftime("%Y%m%dT%H%M%S") + "-routing-" + secrets.token_hex(3))
    directory.mkdir(mode=0o700)
    summary = {
        "started_at": now.isoformat(timespec="seconds"), "base_url": args.base_url.geturl(),
        "model": args.model, "session_field": args.session_field,
        "request_timeout_seconds": args.request_timeout, "passed": False,
        "evidence_dir": str(directory),
    }
    state, suite = None, None
    previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        state = AccountState(Admin(args.base_url, keys[1]))
        summary["initial"] = state.original
        suite = RoutingSuite(args, Runner(args, keys[0]), state)
        summary["checks"] = suite.run()
        summary["passed"] = summary["checks"]["passed"]
    except KeyboardInterrupt:
        summary["failure"] = "interrupted"
    except (OSError, ValueError, RuntimeError, http.client.HTTPException):
        summary["failure"] = "routing_or_management_check_failed"
    finally:
        for sig in previous:
            signal.signal(sig, signal.SIG_IGN)
        if state is not None:
            try:
                drained = state.drain(seconds=args.request_timeout)
                summary["restoration"] = state.restore()
                summary["restoration"]["drained"] = drained
                summary["passed"] &= summary["restoration"]["passed"] and drained
            except (OSError, ValueError, RuntimeError, http.client.HTTPException):
                summary["restoration"] = {"passed": False}
                summary["passed"] = False
        summary["requests"] = suite.reports if suite else []
        summary["generation_requests"] = len(summary["requests"])
        if summary.get("failure"):
            summary["passed"] = False
        json_write(directory / "summary.json", summary)
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    print(json.dumps(summary, ensure_ascii=False))
    return 0 if summary["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

