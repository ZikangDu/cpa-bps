#!/usr/bin/env python3
"""Measure real upstream input-cache usage on one isolated local BPS account."""

import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import signal
import statistics

from real_acceptance import Runner, base_url, model_id
from two_account_acceptance import AccountState, Admin, json_write, require


API_ENV = "CPA_BPS_TEST_API_KEY"
ADMIN_ENV = "CPA_BPS_TEST_ADMIN_KEY"
TOTAL_REQUESTS = 12
GROUPS = ("multi_turn", "exact_repeat", "catalog_repeat")

# Every fact is synthetic. The document is realistic reusable context and all
# benchmark answers are in it; returned tools are never executed.
RUNBOOK = """
Northstar release operations handbook

Purpose and service boundary
Northstar is a fictional package-tracking application used for integration
testing. It has a public read-only tracking endpoint, an authenticated shipment
editor, an asynchronous notification worker, and a reporting console. A release
changes application code while preserving shipment records and audit events.
Operators record the actual results for each action instead of assuming that a
successful process exit proves a healthy deployment. Changes to a subscription,
payment instruction, or shipment destination require their own authorized task.

Release ownership
The release captain is Mira. The database observer is Rowan. The incident
coordinator is Ellis. The captain owns the deployment decision and maintains the
release record. Rowan checks migration progress and storage health. Ellis
coordinates the recovery call and publishes the internal service status. The
captain records a named backup before work begins. A transfer of ownership must
include the current version, the last successful check, the outstanding check,
and any scheduled operation that is still running.

Environment inventory
Development uses the harbor environment, staging uses the meadow environment,
and production uses the summit environment. Each environment has its own
database, message queue, signing material, and deployment identity. A release
record includes the target environment, artifact digest, configuration revision,
database migration revision, and the operator's change identifier. Endpoint
addresses are looked up in the environment inventory instead of copied from
terminal history. Every automated check prints the environment name so that a
correct answer from the wrong environment cannot pass acceptance.

Artifact preparation
The build job produces a versioned application image, a migration bundle, a
dependency manifest, and a checksum list. Images are identified by digest.
The checksum algorithm is SHA256. Staging and production use the same image
digest after acceptance. The release record stores the digest and build job
identifier together. A package rebuilt from the same commit receives a new
record if its output digest changes. Operators verify the manifest before
deployment and again after transferring the package into the target environment.

Configuration preparation
Configuration is stored separately from the artifact and applied as one
validated revision. The candidate revision is parsed and checked against the
new application's supported fields before deployment. Required credentials are
resolved by the runtime service identity. An unset value is treated as missing
when required. A configuration report records field names and validation
outcomes while retaining secret values in the environment credential store.
Operators compare relevant changes and record the approved revision identifier.

Database compatibility
The rollout uses an expand-and-contract migration. New columns and indexes are
introduced first, followed by application code that works with both old and new
records. Removing a legacy column is a later release after all callers have
migrated. Rowan confirms that the migration is resumable and retrying a completed
migration does not duplicate data. A background backfill records its checkpoint.
The migration result includes row counts examined and changed, plus the
checkpoint identifier.

Pre-deployment checks
The captain confirms the artifact digest, approved configuration, and migration
plan before starting the canary. A synthetic shipment is created in staging and
traced through editor, queue, notification worker, and reporting. It carries the
prefix NS_TEST and is removed by its recorded cleanup routine after verification.
Requests are checked at the public entry point used by clients. Failed attempts
and recovery attempts remain separate observations so that release history
reflects the actual sequence of events.

Canary rollout
The canary share is 5 percent. The canary observation window is 15 minutes.
The candidate receives a small portion of ordinary requests while the previous
version remains available for comparison. Operators compare error rates,
response latency, queue depth, and successful shipment updates. A healthy process
alone does not complete the canary. The observation window starts when the first
candidate request is served. The captain records the start time and final
decision, including the metrics supporting that decision.

Promotion criteria
The candidate must pass public tracking, authenticated update, notification
delivery, and report consistency checks. There must be no unexplained increase
in failures or sustained queue growth. Latency measurements use the same
request categories and time window for both versions. If the canary finishes
during an unrelated incident, the captain records that fact and waits for a
clean observation period. Promotion is recorded with the operator, image digest,
configuration revision, and time.

Request tracing
Every synthetic acceptance request uses a unique correlation identifier linking
the client result, gateway record, and worker result. Traces retain timing,
stage, status, and stable error classification. Business text and authentication
material are excluded from acceptance reports. Worker retries retain the original
operation identifier and increase the attempt counter. A successful client
response corresponds to a completed operation. The reporting console shows the
latest successful trace and unresolved failures for each release.

Queue behavior
Notification work has a per-tenant limit. Excess work waits in arrival order.
Canceled requests release their slots, and completed requests release their slots
exactly once. The queue wait timeout is 120 seconds. This timeout applies to
admission and is recorded separately from provider timeouts. Changing a tenant
limit preserves admitted work. After a load test, operators verify active and
waiting counts return to zero and that a fresh notification completes normally.

Cache behavior
Tracking responses use a short-lived application cache only when the record
version is part of the key. Shipment edits invalidate tracking entries after
the committed database change. A cache test checks a cold lookup, a repeated
lookup, an edit, and a lookup after the edit. Observations record the returned
version and actual cache metric. Warm and cold latency are reported separately.
A missing metric stays unknown. Freshness is checked using a modified record,
not inferred from repeatedly receiving a constant answer.

Cancellation and recovery
A client may disconnect after the first part of a streamed report. The gateway
propagates cancellation, the worker releases resources, and the queue returns
to its prior state. Recovery sends a new synthetic request and requires a complete
result. Partial responses remain recorded as partial even when expected text
has arrived. Operators keep the first failure when a later request succeeds.
The recovery record identifies which request completed and which was canceled.

Rollback preparation
The rollback trigger is a sustained error rate above 2 percent for 5 minutes.
Before deployment, the captain records the prior image digest and last approved
configuration. Rollback restores them together. Expand migrations remain
compatible with the previous application. A database restore requires a separate
decision because it may discard committed changes. The plan includes an
independent public endpoint check and synthetic shipment update to confirm the
restored version is serving ordinary traffic.

Rollback execution
Ellis announces rollback in the incident record, the captain restores the
recorded application and configuration revisions, and Rowan verifies storage
health. The team repeats tracking, editing, queue delivery, and reporting
checks. The rollback completion code is ROLLBACK_VERIFIED. It is recorded only
after all four checks finish. If one fails, the incident remains open with
an owner and next diagnostic action. Both deployment records remain available
for review.

Monitoring and escalation
The dashboard shows requests, errors, latency percentiles, worker throughput,
queue depth, database connections, and capacity. The captain uses the relevant
environment and time interval. Ellis opens an incident when health cannot be
confirmed within the planned window. Escalation records the last known good
version, symptoms, affected entry point, and first failing trace. A root cause
is recorded after evidence distinguishes it from other possible causes.
Requests are grouped by operation and environment before comparing trends.

Release communication
The completion note names the deployed version and environment, lists passed
checks, and describes known limitations affecting users. It links the artifact,
configuration, and acceptance results. A team that missed the rollout should
understand the outcome from this note. Any remaining background job has an
explicit owner. A release remains in progress while a required acceptance
check or migration milestone is pending. The note preserves changes in direction
when they explain the final deployment decision.

Post-release review
The review compares expectations with observed behavior, including cold and warm
cache measurements, canary timing, retries or rollback, and final resource
counters. Proposed changes become individual work items with owners and
acceptance criteria. The fixture review label is NS_RELEASE_REVIEW. Synthetic
records are removed after evidence is retained. The final checklist confirms
test identities, temporary limits, and routing changes have returned to their
documented starting values. Reviewers can follow the retained trace identifiers
to distinguish an application defect from a test fixture defect.
""".strip()

QUESTIONS = (
    ("Return only the release captain's first name.", "Mira"),
    ("Return only the production environment name.", "summit"),
    ("Return only the artifact checksum algorithm.", "SHA256"),
    ("Return only the canary share as digits followed by a percent sign.", "5%"),
    ("Return only the rollback completion code.", "ROLLBACK_VERIFIED"),
    ("Return only the fixture review label.", "NS_RELEASE_REVIEW"),
)

TOOLS = [
    {"type": "function", "name": "read_release_status",
     "description": "Read release status when the handbook does not supply the answer.",
     "parameters": {"type": "object", "properties": {"release": {"type": "string"}},
                    "required": ["release"], "additionalProperties": False}},
    {"type": "function", "name": "read_manifest_digest",
     "description": "Read an artifact manifest when a live digest is requested.",
     "parameters": {"type": "object", "properties": {"artifact": {"type": "string"}},
                    "required": ["artifact"], "additionalProperties": False}},
    {"type": "function", "name": "read_rollback_receipt",
     "description": "Read a rollback receipt when live evidence is requested.",
     "parameters": {"type": "object", "properties": {"rollout": {"type": "string"}},
                    "required": ["rollout"], "additionalProperties": False}},
]


def nonnegative_integer(value):
    return isinstance(value, int) and not isinstance(value, bool) and value >= 0


def cache_usage(response):
    """Absent counters stay null, and writes remain separate from cache reads."""
    usage = response.get("usage") if isinstance(response, dict) else None
    usage = usage if isinstance(usage, dict) else {}
    detail = usage.get("input_tokens_details")
    detail = detail if isinstance(detail, dict) else {}
    result = {}
    for field, source in (("input_tokens", usage), ("cached_tokens", detail),
                          ("cache_write_tokens", detail)):
        value = source.get(field)
        result[field] = value if nonnegative_integer(value) else None
    writes = detail.get("cache_write_tokens_details")
    if isinstance(writes, dict):
        result["cache_write_tokens_details"] = {
            name: value for name, value in writes.items()
            if isinstance(name, str) and re.fullmatch(r"ttl_[1-9][0-9]{0,5}[smh]_tokens", name)
            and nonnegative_integer(value)
        }
    total, cached = result["input_tokens"], result["cached_tokens"]
    result["cache_counters_valid"] = total is not None and cached is not None and cached <= total
    result["hit_percent"] = (
        round(100 * cached / total, 4)
        if result["cache_counters_valid"] and total > 0 else None
    )
    return result


def aggregate(reports):
    measured = [row for row in reports if row["cache_usage"]["cache_counters_valid"]]
    total = sum(row["cache_usage"]["input_tokens"] for row in measured)
    cached = sum(row["cache_usage"]["cached_tokens"] for row in measured)
    writes = [row["cache_usage"]["cache_write_tokens"] for row in reports
              if row["cache_usage"]["cache_write_tokens"] is not None]
    write_details = {}
    for report in reports:
        for field, count in report["cache_usage"].get("cache_write_tokens_details", {}).items():
            bucket = write_details.setdefault(field, {"tokens": 0, "requests_with_counter": 0})
            bucket["tokens"] += count
            bucket["requests_with_counter"] += 1
    latencies = [row["duration_ms"] for row in reports if "duration_ms" in row]
    first_text = [row["first_text_ms"] for row in reports if "first_text_ms" in row]
    return {
        "request_count": len(reports),
        "completed_requests": sum(row.get("completed") is True for row in reports),
        "requests_with_cache_counters": len(measured),
        "requests_missing_or_invalid_cache_counters": len(reports) - len(measured),
        "input_tokens_measured": total if measured else None,
        "cached_tokens_measured": cached if measured else None,
        "weighted_hit_percent": round(100 * cached / total, 4) if total > 0 else None,
        "requests_with_any_cache": sum(row["cache_usage"]["cached_tokens"] > 0 for row in measured),
        "requests_with_write_counter": len(writes),
        "cache_write_tokens_measured": sum(writes) if writes else None,
        "cache_write_tokens_details": write_details,
        "median_duration_ms": round(statistics.median(latencies)) if latencies else None,
        "median_first_text_ms": round(statistics.median(first_text)) if first_text else None,
    }


class CacheSuite:
    def __init__(self, args, runner):
        self.args, self.runner = args, runner
        self.request_count = 0

    def call(self, group, index, history, expected, instructions, session, **options):
        require(self.request_count < TOTAL_REQUESTS, "cache benchmark request budget exhausted")
        self.request_count += 1
        if self.args.cache_key_mode == "explicit":
            options["prompt_cache_key"] = session
        report, response, text = self.runner.call(
            group + "_" + str(index + 1), history, stream=True,
            instructions=instructions, **options,
        )
        report.update({
            "position": index + 1, "group": group,
            "cache_stage": "first_observation" if index == 0 else "warm_candidate",
            "selected_account": "account_A",
            "selection_basis": "only_enabled_bps_account",
            "cache_key_mode": self.args.cache_key_mode,
            "input_bytes": len(json.dumps(history, ensure_ascii=False).encode()),
            "instructions_bytes": len(instructions.encode()),
            "cache_usage": cache_usage(response),
            "expected_answer_matched": text.strip() == expected,
        })
        report["passed"] = bool(report.get("completed") and report["expected_answer_matched"]
                                and report.get("text_consistent") is True and not report.get("failure"))
        return report, response

    def run_group(self, group):
        session = "BPS_CACHE_" + secrets.token_hex(8).upper()
        instructions = (
            "Session " + session + ". Use the following synthetic release handbook to answer "
            "the user's questions. All requested answers are present in this document. "
            "Return only the requested answer and do not call a tool.\n\n" + RUNBOOK
        )
        history, reports = [], []
        count = 6 if group == "multi_turn" else 3
        for index in range(count):
            question, expected = QUESTIONS[index] if group == "multi_turn" else QUESTIONS[0]
            if index == 0 or group != "multi_turn":
                question = "Benchmark session " + session + ". " + question
            current = history + [{"role": "user", "content": question}] if group == "multi_turn" else [
                {"role": "user", "content": question}]
            options = {}
            if group == "catalog_repeat":
                rotation = index % len(TOOLS)
                options = {"tools": TOOLS[rotation:] + TOOLS[:rotation], "tool_choice": "auto"}
            report, response = self.call(group, index, current, expected, instructions, session, **options)
            reports.append(report)
            if not report["passed"]:
                break
            if group == "multi_turn":
                history = current + response["output"]
        return {
            "case": group,
            "passed": len(reports) == count and all(row["passed"] for row in reports),
            "expected_requests": count,
            "requests": reports,
            "all": aggregate(reports),
            "first": aggregate(reports[:1]),
            "warm": aggregate(reports[1:]),
        }


def interrupted(_signal, _frame):
    raise KeyboardInterrupt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", type=base_url, required=True)
    parser.add_argument("--model", type=model_id, default="gpt-6.2-sol")
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--phase", choices=("baseline", "optimized"), required=True)
    parser.add_argument("--cache-key-mode", choices=("automatic", "explicit"), default="automatic")
    parser.add_argument("--request-timeout", type=int, choices=range(15, 121), default=60, metavar="15..120")
    args = parser.parse_args()
    keys = [os.environ.get(name, "") for name in (API_ENV, ADMIN_ENV)]
    if any(not key or any(ord(char) <= 32 or ord(char) >= 127 for char in key) for key in keys):
        parser.error("the local client and management key environment variables are required")

    args.evidence.mkdir(parents=True, exist_ok=True)
    now = datetime.now(timezone(timedelta(hours=8)))
    directory = args.evidence / (now.strftime("%Y%m%dT%H%M%S") + "-" + args.phase + "-" + secrets.token_hex(3))
    directory.mkdir(mode=0o700)
    summary = {
        "started_at": now.isoformat(timespec="seconds"),
        "phase": args.phase, "base_url": args.base_url.geturl(), "model": args.model,
        "cache_key_mode": args.cache_key_mode, "request_timeout_seconds": args.request_timeout,
        "fixture_sha256": hashlib.sha256(RUNBOOK.encode()).hexdigest(),
        "fixture_words": len(RUNBOOK.split()), "fixture_bytes": len(RUNBOOK.encode()),
        "measurement": "upstream usage.input_tokens_details.cached_tokens / usage.input_tokens",
        "first_observation": "Fresh session marker; server-side cache coldness is not assumed.",
        "cases": [], "passed": False, "evidence_dir": str(directory),
    }
    previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
    state, suite = None, None
    all_reports, warm_reports = [], []
    try:
        state = AccountState(Admin(args.base_url, keys[1]))
        summary["initial"] = state.original
        require(state.drain(), "existing account requests did not drain")
        state.select(["account_A"])
        suite = CacheSuite(args, Runner(args, keys[0]))
        for index, group in enumerate(GROUPS):
            result = suite.run_group(group)
            all_reports.extend(result["requests"])
            warm_reports.extend(result["requests"][1:])
            item = {"case": group, "passed": result["passed"], "all": result["all"], "warm": result["warm"]}
            summary["cases"].append(item)
            json_write(directory / (f"{index+1:02d}_" + group + ".json"), result)
            print(json.dumps(item, ensure_ascii=False), flush=True)
            if not result["passed"]:
                summary["stopped_after"] = group
                break
        summary["generation_requests"] = suite.request_count
        summary["passed"] = len(summary["cases"]) == len(GROUPS) and all(row["passed"] for row in summary["cases"])
    except KeyboardInterrupt:
        summary["failure"] = "interrupted"
    except (OSError, ValueError, RuntimeError, http.client.HTTPException):
        summary["failure"] = "local_management_or_benchmark_failure"
    finally:
        for sig in previous:
            signal.signal(sig, signal.SIG_IGN)
        if suite is not None:
            summary["generation_requests"] = suite.request_count
        if state is not None:
            try:
                drained = state.drain(seconds=args.request_timeout)
                summary["restoration"] = state.restore()
                summary["restoration"]["drained"] = drained
                summary["passed"] &= summary["restoration"]["passed"] and drained
            except (OSError, ValueError, RuntimeError, http.client.HTTPException):
                summary["restoration"] = {"passed": False}
                summary["passed"] = False
        if summary.get("failure"):
            summary["passed"] = False
        summary["all"] = aggregate(all_reports)
        summary["warm"] = aggregate(warm_reports)
        json_write(directory / "summary.json", summary)
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    print(json.dumps(summary, ensure_ascii=False))
    return 0 if summary["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

