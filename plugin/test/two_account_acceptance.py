#!/usr/bin/env python3
"""Verify two local BPS accounts and restore temporary account/queue settings."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from datetime import datetime, timedelta, timezone
import http.client
import json
import math
import os
from pathlib import Path
import secrets
import signal
import statistics
import threading
import time
from urllib.parse import urlencode

from real_acceptance import Runner, RED_PNG, base_url, marker, model_id


API_ENV = "CPA_BPS_TEST_API_KEY"
ADMIN_ENV = "CPA_BPS_TEST_ADMIN_KEY"
CASES = ("accounts", "cross_text", "cross_tool", "cross_image", "long_context",
         "structured", "queue", "concurrency")
MAX_GENERATIONS = 60


def require(value, message):
    if not value:
        raise RuntimeError(message)


def json_write(path, value):
    with path.open("x", encoding="utf-8") as file:
        json.dump(value, file, ensure_ascii=False, indent=2)
        file.write("\n")


class Admin:
    def __init__(self, base, key):
        self.base, self.key = base, key

    def call(self, path, method="GET", body=None):
        cls = http.client.HTTPSConnection if self.base.scheme == "https" else http.client.HTTPConnection
        with closing(cls(self.base.hostname, self.base.port, timeout=8)) as client:
            client.request(method, path, None if body is None else json.dumps(body).encode(),
                           {"Authorization": "Bearer " + self.key, "Content-Type": "application/json"})
            with client.getresponse() as response:
                raw = response.read(2 << 20)
                if response.status != 200:
                    raise RuntimeError(f"local management operation returned HTTP {response.status}")
                parsed = json.loads(raw)
                require(isinstance(parsed, dict), "local management returned an unexpected object")
                return parsed

    def credentials(self):
        values = self.call("/v8/management/credentials").get("files")
        require(isinstance(values, list), "credential listing is unavailable")
        return values

    def set_disabled(self, account, disabled):
        self.call("/v8/management/credentials/status", "PATCH",
                  {"name": account["name"], "auth_index": account["auth_index"], "disabled": disabled})

    def concurrency(self, account, limit=None):
        path = "/v0/management/cpa-bps/concurrency?" + urlencode({"auth_index": account["auth_index"]})
        data = self.call(path) if limit is None else self.call(path, "POST", {"limit": limit})
        require(all(isinstance(data.get(key), int) and not isinstance(data[key], bool)
                    and data[key] >= 0 for key in ("active", "waiting", "limit")),
                "the account concurrency endpoint returned invalid counters")
        return {key: data[key] for key in ("active", "waiting", "limit")}


class AccountState:
    def __init__(self, admin):
        self.admin = admin
        records = [row for row in admin.credentials()
                   if "cpa-bps" in (row.get("provider"), row.get("type"))]
        require(len(records) == 2, "this acceptance run requires exactly two local BPS accounts")
        require(all(isinstance(row.get("name"), str) and row["name"]
                    and isinstance(row.get("auth_index"), str) and row["auth_index"] for row in records),
                "both local accounts need stable names and indexes")
        records.sort(key=lambda row: row["name"])
        self.accounts = {}
        for label, row in zip(("account_A", "account_B"), records):
            self.accounts[label] = {
                "name": row["name"], "auth_index": row["auth_index"],
                "disabled": bool(row.get("disabled")),
                "limit": admin.concurrency(row)["limit"],
            }
        require(all(not row["disabled"] for row in self.accounts.values()),
                "both test accounts must initially be enabled")
        self.original = {label: {key: row[key] for key in ("disabled", "limit")}
                         for label, row in self.accounts.items()}
        self.restored = None

    def current(self):
        by_index = {row.get("auth_index"): row for row in self.admin.credentials()}
        require(all(account["auth_index"] in by_index for account in self.accounts.values()),
                "a test account disappeared during acceptance")
        return {label: by_index[account["auth_index"]] for label, account in self.accounts.items()}

    def select(self, labels):
        """Select through the supported administrative disabled-state contract."""
        wanted = set(labels)
        current = self.current()
        # Stop unselected accounts first, then enable requested accounts.
        for disabled in (True, False):
            for label, account in self.accounts.items():
                target = label not in wanted
                if target == disabled and bool(current[label].get("disabled")) != target:
                    self.admin.set_disabled(account, target)
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            current = self.current()
            if all(bool(current[label].get("disabled")) == (label not in wanted) for label in current):
                # File-watcher model publication and scheduler updates follow the mutation.
                time.sleep(0.3)
                return
            time.sleep(0.15)
        raise RuntimeError("local account selection did not converge")

    def set_limits(self, limit):
        for account in self.accounts.values():
            require(self.admin.concurrency(account, limit)["limit"] == limit,
                    "the requested concurrency limit was not acknowledged")

    def queues(self):
        return {label: self.admin.concurrency(account) for label, account in self.accounts.items()}

    def drain(self, seconds=45):
        deadline = time.monotonic() + seconds
        last = self.queues()
        while any(row["active"] or row["waiting"] for row in last.values()):
            if time.monotonic() >= deadline:
                return False
            time.sleep(0.25)
            last = self.queues()
        return True

    def restore(self):
        failures = []
        for label, account in self.accounts.items():
            try:
                self.admin.concurrency(account, self.original[label]["limit"])
            except (OSError, ValueError, RuntimeError, http.client.HTTPException):
                failures.append(label + "_limit")
        for label, account in self.accounts.items():
            try:
                self.admin.set_disabled(account, self.original[label]["disabled"])
            except (OSError, ValueError, RuntimeError, http.client.HTTPException):
                failures.append(label + "_status")
        try:
            states, queues = self.current(), self.queues()
            matched = all(bool(states[label].get("disabled")) == original["disabled"]
                          and queues[label]["limit"] == original["limit"]
                          for label, original in self.original.items())
            final = {label: {"disabled": bool(states[label].get("disabled")), **queues[label]}
                     for label in self.accounts}
        except (OSError, ValueError, RuntimeError, http.client.HTTPException):
            matched, final = False, {}
        self.restored = {"passed": matched and not failures, "failed_operations": failures,
                         "final": final}
        return self.restored


class Suite:
    def __init__(self, args, runner, state):
        self.args, self.runner, self.state = args, runner, state
        self.request_count = 0
        self.lock = threading.Lock()
        self.stop_waves = False

    def call(self, name, input_value, *, stream=True, **options):
        with self.lock:
            require(self.request_count < MAX_GENERATIONS, "generation budget exhausted")
            self.request_count += 1
        return self.runner.call(name, input_value, stream=stream, **options)

    def exact(self, name, expected, input_value=None, *, account=None, stream=True, **options):
        prompt = input_value if input_value is not None else "Reply with exactly " + expected + " and nothing else."
        evidence, response, text = self.call(name, prompt, stream=stream, **options)
        evidence["marker_matched"] = text.strip() == expected
        evidence["passed"] = bool(evidence["completed"] and evidence["marker_matched"]
                                  and not evidence.get("failure")
                                  and (not stream or evidence.get("text_consistent") is True))
        if account:
            evidence["selected_account"] = account
            evidence["selection_basis"] = "only_enabled_bps_account"
        return evidence, response

    @staticmethod
    def result(case, reports, **fields):
        return {"case": case, "passed": bool(reports) and all(row.get("passed") for row in reports),
                "request_count": len(reports), "requests": reports, **fields}

    def accounts(self):
        reports = []
        for label in self.state.accounts:
            self.state.select([label])
            for stream in (False, True):
                report, _ = self.exact(label + ("_sse" if stream else "_json"), marker(),
                                       account=label, stream=stream)
                reports.append(report)
                if not report["passed"]:
                    return self.result("accounts", reports, prerequisites_failed=True)
        return self.result("accounts", reports)

    def cross_text(self):
        expected = marker()
        prompt = "Remember this exact code for this conversation: " + expected + ". Reply only READY."
        self.state.select(["account_A"])
        first, response = self.exact("cross_text_open", "READY", prompt, account="account_A")
        reports = [first]
        if first["passed"]:
            history = [{"role": "user", "content": prompt}] + response["output"]
            for label in ("account_B", "account_A"):
                self.state.select([label])
                current = history + [{"role": "user", "content": "Reply only with the exact code I asked you to remember."}]
                answer, response = self.exact("cross_text_recall", expected, current, account=label)
                reports.append(answer)
                if not answer["passed"]:
                    break
                history = current + response["output"]
        return self.result("cross_text", reports, completed_switches=len(reports)-1,
                           expected_switches=2, expected_marker=expected)

    def cross_tool(self):
        expected, result_marker = marker(), marker()
        tool = {"type": "function", "name": "fixture_lookup",
                "description": "Synthetic test data lookup.",
                "parameters": {"type": "object", "properties": {"value": {"type": "string"}},
                               "required": ["value"], "additionalProperties": False}, "strict": True}
        prompt = "Call fixture_lookup once with value " + expected + " and wait for its result."
        self.state.select(["account_A"])
        first, response, _ = self.call("cross_tool_call", prompt, tools=[tool],
                                       tool_choice={"type": "function", "name": "fixture_lookup"})
        calls = [item for item in (response or {}).get("output", [])
                 if isinstance(item, dict) and item.get("type") == "function_call"]
        call = calls[0] if len(calls) == 1 else {}
        try:
            arguments = json.loads(call.get("arguments", ""))
        except (ValueError, TypeError):
            arguments = None
        first["tool_contract_matched"] = bool(call.get("name") == "fixture_lookup"
                                              and arguments == {"value": expected}
                                              and isinstance(call.get("call_id"), str) and call["call_id"])
        first["selected_account"] = "account_A"
        first["passed"] = first["completed"] and first["tool_contract_matched"] and not first.get("failure")
        reports = [first]
        if first["passed"]:
            self.state.select(["account_B"])
            history = [{"role": "user", "content": prompt}] + response["output"] + [
                {"type": "function_call_output", "call_id": call["call_id"], "output": result_marker},
                {"role": "user", "content": "Reply only with the exact tool result. Do not call a tool again."}]
            answer, _ = self.exact("cross_tool_result", result_marker, history, account="account_B",
                                   tools=[tool], tool_choice="none")
            reports.append(answer)
        result = self.result("cross_tool", reports, tool_execution="synthetic_result_only",
                             expected_requests=2)
        result["passed"] &= len(reports) == 2
        return result

    def cross_image(self):
        first_prompt = [{"role": "user", "content": [
            {"type": "input_text", "text": "What primary color fills this image? Reply only with its lowercase English color."},
            {"type": "input_image", "image_url": "data:image/png;base64," + RED_PNG, "detail": "low"},
        ]}]
        self.state.select(["account_A"])
        first, response = self.exact("cross_image_open", "red", first_prompt, account="account_A")
        reports = [first]
        if first["passed"]:
            self.state.select(["account_B"])
            history = first_prompt + response["output"] + [
                {"role": "user", "content": "Which color was shown in my image? Reply only with the lowercase color."}]
            second, second_response = self.exact("cross_image_recall", "red", history, account="account_B")
            reports.append(second)
            if second["passed"]:
                self.state.select(["account_A"])
                history += second_response["output"] + [
                    {"role": "user", "content": "Return only the original image color once more."}]
                third, _ = self.exact("cross_image_return_to_first_account", "red", history,
                                      account="account_A")
                reports.append(third)
        result = self.result("cross_image", reports, fixture="64x64_rgb_red_png_full_history_A_B_A",
                             expected_requests=3)
        result["passed"] &= len(reports) == 3
        return result

    def long_context(self):
        expected = marker()
        history = []
        notes = ("The synthetic logistics ledger records harmless package labels, garden observations, "
                 "library shelf names and classroom schedules. Every entry is test data. Keep the "
                 "original conversation code available while reading the complete history. ")
        for turn in range(12):
            lead = ("The exact conversation code is " + expected + ". ") if turn == 0 else ""
            history.append({"role": "user", "content": lead + f"Record {turn+1}. " + notes * 9})
            history.append({"role": "assistant", "content": f"Recorded synthetic entry {turn+1}."})
        history.append({"role": "user", "content": "Reply only with the exact original conversation code."})
        reports = []
        for label in ("account_A", "account_B"):
            self.state.select([label])
            report, _ = self.exact("long_context_recall", expected, history, account=label)
            reports.append(report)
        return self.result("long_context", reports, context_turns=12,
                           input_bytes=len(json.dumps(history).encode()), expected_marker=expected)

    def structured(self):
        reports = []
        for label in self.state.accounts:
            self.state.select([label])
            expected = marker()
            schema = {"type": "json_schema", "name": "account_marker", "strict": True,
                      "schema": {"type": "object", "properties": {"marker": {"type": "string"}},
                                 "required": ["marker"], "additionalProperties": False}}
            evidence, _, text = self.call("structured_" + label,
                                         "Return JSON with marker equal to " + expected + ".",
                                         stream=False, text={"format": schema})
            try:
                valid = json.loads(text) == {"marker": expected}
            except ValueError:
                valid = False
            evidence.update({"selected_account": label, "schema_matched": valid,
                             "passed": evidence["completed"] and valid and not evidence.get("failure")})
            reports.append(evidence)
        return self.result("structured", reports)

    def wave(self, width, per_account_limit, name):
        self.state.select(self.state.accounts)
        require(self.state.drain(), "previous requests did not drain before concurrency test")
        self.state.set_limits(per_account_limit)
        barrier = threading.Barrier(width)
        stop = threading.Event()
        peak = {label: {"active": 0, "waiting": 0} for label in self.state.accounts}
        max_total_active = 0
        poll_errors = 0

        def observe():
            nonlocal max_total_active, poll_errors
            while not stop.wait(0.1):
                try:
                    snapshot = self.state.queues()
                    max_total_active = max(max_total_active, sum(value["active"] for value in snapshot.values()))
                    for label, value in snapshot.items():
                        for key in ("active", "waiting"):
                            peak[label][key] = max(peak[label][key], value[key])
                except (OSError, ValueError, RuntimeError, http.client.HTTPException):
                    poll_errors += 1

        def request(index):
            expected = marker()
            barrier.wait(timeout=8)
            evidence, _ = self.exact(name + "_" + str(index), expected)
            evidence["expected_marker"] = expected
            return evidence

        observer = threading.Thread(target=observe, daemon=True)
        observer.start()
        start = time.monotonic()
        try:
            with ThreadPoolExecutor(max_workers=width) as pool:
                reports = list(pool.map(request, range(width)))
        finally:
            stop.set()
            observer.join(timeout=8)
        drained = self.state.drain()
        final = self.state.queues()
        latencies = sorted(row["duration_ms"] for row in reports)
        failures = sum(not row["passed"] for row in reports)
        limited = any(row.get("http") == 429 or
                      row.get("terminal_error", {}).get("code") in {"rate_limit_exceeded", "insufficient_quota"}
                      for row in reports)
        if limited or failures >= max(1, math.ceil(width * 0.2)) or not drained:
            self.stop_waves = True
        return {"width": width, "per_account_limit": per_account_limit,
                "passed": failures == 0 and drained, "successes": width-failures, "failures": failures,
                "duration_ms": round((time.monotonic()-start)*1000),
                "median_request_ms": round(statistics.median(latencies)), "p95_request_ms": latencies[math.ceil(width*.95)-1],
                "peak": peak, "max_total_active": max_total_active, "final": final,
                "routing_observation": "per_account_active_counters",
                "accounts_observed_active": [label for label, values in peak.items() if values["active"] > 0],
                "counter_poll_errors": poll_errors, "drained": drained,
                "requests": reports, "stop_escalation": self.stop_waves}

    def queue(self):
        wave = self.wave(5, 1, "queue")
        wave["queue_observed"] = any(value["waiting"] > 0 for value in wave["peak"].values())
        return {"case": "queue", "passed": wave["passed"] and wave["queue_observed"],
                "request_count": len(wave["requests"]), "waves": [wave]}

    def concurrency(self):
        waves = []
        for width in self.args.waves:
            if self.stop_waves:
                break
            wave = self.wave(width, max(1, math.ceil(width/2)), "concurrency_" + str(width))
            waves.append(wave)
            if wave["stop_escalation"]:
                break
        return {"case": "concurrency", "passed": len(waves) == len(self.args.waves) and all(x["passed"] for x in waves),
                "request_count": sum(len(x["requests"]) for x in waves),
                "requested_waves": self.args.waves, "waves": waves}


def interrupted(_signum, _frame):
    raise KeyboardInterrupt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", type=base_url, required=True)
    parser.add_argument("--model", type=model_id, default="gpt-6.2-sol")
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--case", choices=CASES, action="append")
    parser.add_argument("--request-timeout", type=int, default=45, choices=range(15, 121),
                        metavar="15..120", help="per-request observation deadline in seconds")
    parser.add_argument("--waves", default="2,5,10",
                        help="increasing simultaneous client counts, each 1..20, sum at most 40")
    args = parser.parse_args()
    try:
        args.waves = [int(value) for value in args.waves.split(",")]
        if not args.waves or any(x < 1 or x > 20 for x in args.waves) or args.waves != sorted(set(args.waves)) or sum(args.waves) > 40:
            raise ValueError
    except ValueError:
        parser.error("--waves must be distinct increasing integers from 1 through 20, totaling at most 40")
    keys = [os.environ.get(name, "") for name in (API_ENV, ADMIN_ENV)]
    if any(not key or any(ord(char) <= 32 or ord(char) >= 127 for char in key) for key in keys):
        parser.error("the local client and management key environment variables are required")
    args.evidence.mkdir(parents=True, exist_ok=True)
    now = datetime.now(timezone(timedelta(hours=8)))
    directory = args.evidence / (now.strftime("%Y%m%dT%H%M%S") + "-" + secrets.token_hex(3))
    directory.mkdir(mode=0o700)
    summary = {"started_at": now.isoformat(timespec="seconds"), "base_url": args.base_url.geturl(),
               "model": args.model, "selection_method": "supported_local_disabled_state",
               "request_timeout_seconds": args.request_timeout,
               "cases": [], "passed": False, "evidence_dir": str(directory)}
    state = None
    current_case = None
    previous_signals = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGTERM, signal.SIGINT)}
    try:
        state = AccountState(Admin(args.base_url, keys[1]))
        summary["initial"] = state.original
        suite = Suite(args, Runner(args, keys[0]), state)
        for name in args.case or CASES:
            if suite.stop_waves and name in {"queue", "concurrency"}:
                summary["stopped_before"] = name
                break
            current_case = name
            result = getattr(suite, name)()
            item = {"case": name, "passed": result["passed"], "request_count": result["request_count"]}
            summary["cases"].append(item)
            json_write(directory / (f"{len(summary['cases']):02d}_" + name + ".json"), result)
            print(json.dumps(item), flush=True)
            if name == "accounts" and not result["passed"]:
                summary["stopped_before"] = "prerequisite_failed"
                break
        summary["generation_requests"] = suite.request_count
        summary["passed"] = bool(summary["cases"]) and all(item["passed"] for item in summary["cases"]) and not summary.get("stopped_before")
    except KeyboardInterrupt:
        summary["failure"] = "interrupted"
    except (OSError, ValueError, RuntimeError, http.client.HTTPException):
        summary["failure"] = "local_management_or_test_failure"
        summary["failed_case"] = current_case
    finally:
        # Avoid a second Ctrl-C interrupting restoration of the user's local accounts.
        for sig in previous_signals:
            signal.signal(sig, signal.SIG_IGN)
        if state is not None:
            try:
                drained = state.drain()
                summary["restoration"] = state.restore()
                summary["restoration"]["drained"] = drained
                summary["passed"] &= summary["restoration"]["passed"] and drained
            except (OSError, ValueError, RuntimeError, http.client.HTTPException):
                summary["restoration"] = {"passed": False}
                summary["passed"] = False
        if summary.get("failure"):
            summary["passed"] = False
        json_write(directory / "summary.json", summary)
        for sig, handler in previous_signals.items():
            signal.signal(sig, handler)
    print(json.dumps(summary, ensure_ascii=False))
    return 0 if summary["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
