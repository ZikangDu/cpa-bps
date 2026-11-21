#!/usr/bin/env python3
"""Discover both local BPS catalogs and measure two SSE requests per model."""

import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import signal
from types import SimpleNamespace
from urllib.parse import urlencode

from cache_acceptance import RUNBOOK, aggregate, cache_usage
from real_acceptance import Runner, base_url, model_id
from two_account_acceptance import AccountState, Admin, json_write, require


API_ENV = "CPA_BPS_TEST_API_KEY"
ADMIN_ENV = "CPA_BPS_TEST_ADMIN_KEY"
MAX_MODELS = 12
MAX_REQUESTS = MAX_MODELS * 2
EXPECTED_MODELS = 9
SELECTED_ACCOUNT = "account_B"
OPERATIONS_ERRORS = (OSError, ValueError, RuntimeError, http.client.HTTPException)


def catalog_ids(payload, field):
    rows = payload.get(field) if isinstance(payload, dict) else None
    require(isinstance(rows, list), "model catalog has no model list")
    models = []
    for row in rows:
        value = row.get("id") if isinstance(row, dict) else None
        require(isinstance(value, str), "model catalog contains an invalid identifier")
        models.append(model_id(value))
    require(models, "model catalog is empty")
    require(len(models) == len(set(models)), "model catalog contains duplicate identifiers")
    return sorted(models)


def discover_catalogs(state, public_api, expected_count):
    """Credential catalogs prove ownership; public catalogs can be a union."""
    catalogs = {}
    for label, account in state.accounts.items():
        require(state.drain(), "previous local requests did not drain before catalog discovery")
        state.select([label])
        report = {"selected_account": label, "selection_basis": "only_enabled_bps_account"}
        try:
            path = "/v0/management/auth-files/models?" + urlencode({"name": account["name"]})
            report["models"] = catalog_ids(state.admin.call(path), "models")
            report["source"] = "credential_models_api"
            report["account_specific"] = True
        except (*OPERATIONS_ERRORS, argparse.ArgumentTypeError):
            report["credential_catalog_unavailable"] = True
            report["account_specific"] = False
        try:
            public_models = catalog_ids(public_api.call("/v1/models"), "data")
            report["public_models"] = public_models
        except (*OPERATIONS_ERRORS, argparse.ArgumentTypeError):
            public_models = []
            report["public_catalog_unavailable"] = True
        if "models" not in report:
            require(public_models, "neither credential nor public model catalog is available")
            report["models"] = public_models
            report["source"] = "public_models_with_single_enabled_account_possible_union"
        report["count"] = len(report["models"])
        report["expected_count_matched"] = report["count"] == expected_count
        report["missing_from_public_catalog"] = sorted(set(report["models"]) - set(public_models)) if public_models else None
        catalogs[label] = report
    require(set(catalogs) == {"account_A", "account_B"}, "catalog test needs the two local BPS accounts")
    same = catalogs["account_A"]["models"] == catalogs["account_B"]["models"]
    verified = all(row["account_specific"] for row in catalogs.values())
    return {
        "accounts": catalogs,
        "same_observed_catalog": same,
        "account_specific_catalogs_verified": verified,
        "expected_count": expected_count,
        "passed": same and verified and all(row["expected_count_matched"] for row in catalogs.values()),
        "fallback_limitation": (
            None if verified else "/v1/models may contain a union; equal public lists do not prove equal account-specific catalogs."
        ),
    }


class ModelCacheSuite:
    def __init__(self, args, runner, state):
        self.args, self.runner, self.state = args, runner, state
        self.request_count = 0
        self.reports = []

    def run_model(self, model, position):
        require(1 <= position <= MAX_MODELS, "model request budget exhausted")
        session = "BPS_MODEL_CACHE_" + secrets.token_hex(16)
        instructions = (
            "Session " + session + ". Use this synthetic release handbook to answer the user's question. "
            "Return only the requested answer and do not call a tool.\n\n" + RUNBOOK
        )
        history = [{"role": "user", "content": "Benchmark session " + session + ". Return only the release captain's first name."}]
        reports = []
        for index in range(2):
            require(self.request_count < MAX_REQUESTS, "model request budget exhausted")
            require(self.state.drain(seconds=self.args.request_timeout), "previous local request did not drain")
            self.request_count += 1
            options = {"model": model, "instructions": instructions}
            if self.args.cache_key_mode == "explicit":
                options["prompt_cache_key"] = session
            report, response, text = self.runner.call(
                f"model_{position:02d}_observation_{index + 1}", history, stream=True, **options,
            )
            report.update({
                "model": model, "position": index + 1,
                "cache_stage": "first_observation" if index == 0 else "warm_candidate",
                "selected_account": SELECTED_ACCOUNT,
                "selection_basis": "only_enabled_bps_account",
                "cache_key_mode": self.args.cache_key_mode,
                "cache_usage": cache_usage(response),
                "expected_answer_matched": text.strip() == "Mira",
                "instructions_bytes": len(instructions.encode()),
                "input_bytes": len(json.dumps(history, ensure_ascii=False).encode()),
            })
            report["passed"] = bool(
                report.get("completed") and report["expected_answer_matched"]
                and report.get("text_consistent") is True and not report.get("failure")
            )
            reports.append(report)
            self.reports.append(report)
            # Observation two is part of the predefined pair, including when
            # observation one fails. Neither observation is retried or replaced.
        return {
            "model": model, "passed": all(row["passed"] for row in reports),
            "requests": reports, "all": aggregate(reports),
            "cold": aggregate(reports[:1]), "warm": aggregate(reports[1:]),
            "cold_label": "first observation; server cache coldness is not assumed",
        }


def interrupted(_signal, _frame):
    raise KeyboardInterrupt


def self_test():
    """Use synthetic objects only; this entry point never constructs a client."""
    ids = [f"fixture-model-{index}" for index in range(EXPECTED_MODELS)]

    class FixtureAPI:
        def __init__(self, credential_available=True):
            self.credential_available = credential_available

        def call(self, path):
            if path.startswith("/v0/management/auth-files/models?"):
                if not self.credential_available:
                    raise RuntimeError("fixture unavailable")
                return {"models": [{"id": value} for value in ids]}
            require(path == "/v1/models", "unexpected synthetic catalog operation")
            return {"data": [{"id": value} for value in ids]}

    class FixtureState:
        def __init__(self):
            self.admin = FixtureAPI()
            self.accounts = {label: {"name": label + ".json"} for label in ("account_A", "account_B")}

        def drain(self, seconds=45):
            return True

        def select(self, labels):
            require(len(labels) == 1 and labels[0] in self.accounts, "invalid synthetic account selection")

    class FixtureRunner:
        def __init__(self):
            self.calls = []

        def call(self, name, history, *, stream=False, **options):
            self.calls.append((history, stream, options))
            failed = len(self.calls) == 1
            report = {"completed": not failed, "text_consistent": True, "duration_ms": 10, "attempts": 1}
            if failed:
                report["failure"] = "fixture_first_observation_failure"
            response = {"usage": {"input_tokens": 2000, "input_tokens_details": {"cached_tokens": 1000}}}
            return report, response, "Mira"

    state = FixtureState()
    discovery = discover_catalogs(state, FixtureAPI(), EXPECTED_MODELS)
    require(discovery["passed"], "synthetic credential catalogs were not verified")
    state.admin = FixtureAPI(False)
    fallback = discover_catalogs(state, FixtureAPI(), EXPECTED_MODELS)
    require(not fallback["passed"] and not fallback["account_specific_catalogs_verified"], "public union was treated as account ownership proof")
    runner = FixtureRunner()
    suite = ModelCacheSuite(SimpleNamespace(request_timeout=60, cache_key_mode="automatic"), runner, state)
    results = [suite.run_model(model, index + 1) for index, model in enumerate(ids)]
    require(suite.request_count == 18 and len(runner.calls) == 18, "synthetic model request count is wrong")
    require(not results[0]["passed"] and results[0]["requests"][0]["failure"] == "fixture_first_observation_failure", "original failure was lost")
    require(results[0]["requests"][1]["passed"], "the second planned observation was skipped")
    require(all(row["passed"] for row in results[1:]), "synthetic successful models did not pass")
    for index in range(0, len(runner.calls), 2):
        require(runner.calls[index] == runner.calls[index + 1], "the two model prompts differ")
        require(runner.calls[index][1] is True, "a model observation did not request SSE")
    require(aggregate(suite.reports)["weighted_hit_percent"] == 50, "synthetic cache counters were misreported")
    print(json.dumps({"self_test": "passed", "network_requests": 0, "synthetic_generation_requests": suite.request_count}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true", help="run synthetic checks without network access or credentials")
    parser.add_argument("--base-url", type=base_url)
    parser.add_argument("--evidence", type=Path)
    parser.add_argument("--expected-model-count", type=int, choices=range(1, MAX_MODELS + 1), default=EXPECTED_MODELS)
    parser.add_argument("--cache-key-mode", choices=("automatic", "explicit"), default="automatic")
    parser.add_argument("--request-timeout", type=int, choices=range(15, 121), default=60, metavar="15..120")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return 0
    if args.base_url is None or args.evidence is None:
        parser.error("--base-url and --evidence are required for live acceptance")
    keys = [os.environ.get(name, "") for name in (API_ENV, ADMIN_ENV)]
    if any(not key or any(ord(char) <= 32 or ord(char) >= 127 for char in key) for key in keys):
        parser.error("the local client and management key environment variables are required")
    # Runner supplies this default field; each request overrides it with its
    # discovered model ID. No configured or hard-coded catalog is used.
    args.model = "discovered-model-overridden-per-request"
    args.evidence.mkdir(parents=True, exist_ok=True)
    now = datetime.now(timezone(timedelta(hours=8)))
    directory = args.evidence / (now.strftime("%Y%m%dT%H%M%S") + "-model-cache-" + secrets.token_hex(3))
    directory.mkdir(mode=0o700)
    summary = {
        "started_at": now.isoformat(timespec="seconds"), "base_url": args.base_url.geturl(),
        "selected_account": SELECTED_ACCOUNT, "cache_key_mode": args.cache_key_mode,
        "request_timeout_seconds": args.request_timeout, "generation_request_budget": MAX_REQUESTS,
        "fixture_sha256": hashlib.sha256(RUNBOOK.encode()).hexdigest(),
        "fixture_words": len(RUNBOOK.split()), "fixture_bytes": len(RUNBOOK.encode()),
        "measurement": "upstream usage.input_tokens_details.cached_tokens / usage.input_tokens",
        "cold_label": "Fresh session marker; server-side cache coldness is not assumed.",
        "models": [], "passed": False, "evidence_dir": str(directory),
    }
    previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
    state, suite = None, None
    try:
        state = AccountState(Admin(args.base_url, keys[1]))
        summary["initial"] = state.original
        discovery = discover_catalogs(state, Admin(args.base_url, keys[0]), args.expected_model_count)
        summary["catalog_discovery"] = discovery
        json_write(directory / "catalogs.json", discovery)
        discovered = discovery["accounts"][SELECTED_ACCOUNT]["models"]
        models = discovered[:MAX_MODELS]
        summary["omitted_models_due_to_budget"] = discovered[MAX_MODELS:]
        state.select([SELECTED_ACCOUNT])
        require(state.admin.concurrency(state.accounts[SELECTED_ACCOUNT], 1)["limit"] == 1,
                "selected-account sequential limit was not acknowledged")
        suite = ModelCacheSuite(args, Runner(args, keys[0]), state)
        for index, model in enumerate(models):
            result = suite.run_model(model, index + 1)
            json_write(directory / f"{index + 1:02d}_model.json", result)
            item = {key: result[key] for key in ("model", "passed", "all", "cold", "warm")}
            summary["models"].append(item)
            print(json.dumps(item, ensure_ascii=False), flush=True)
        summary["passed"] = bool(
            discovery["passed"] and not summary["omitted_models_due_to_budget"]
            and len(summary["models"]) == len(discovered) and all(row["passed"] for row in summary["models"])
        )
    except KeyboardInterrupt:
        summary["failure"] = "interrupted"
    except (*OPERATIONS_ERRORS, argparse.ArgumentTypeError):
        summary["failure"] = "local_management_or_model_benchmark_failure"
    finally:
        for sig in previous:
            signal.signal(sig, signal.SIG_IGN)
        reports = suite.reports if suite is not None else []
        summary["generation_requests"] = suite.request_count if suite is not None else 0
        if state is not None:
            try:
                drained = state.drain(seconds=args.request_timeout)
            except OPERATIONS_ERRORS:
                drained = False
            try:
                summary["restoration"] = state.restore()
                summary["restoration"]["drained"] = drained
                summary["passed"] &= summary["restoration"]["passed"] and drained
            except OPERATIONS_ERRORS:
                summary["restoration"] = {"passed": False, "drained": drained}
                summary["passed"] = False
        summary["passed"] &= not bool(summary.get("failure"))
        summary["all"] = aggregate(reports)
        summary["cold"] = aggregate([row for row in reports if row["position"] == 1])
        summary["warm"] = aggregate([row for row in reports if row["position"] == 2])
        # Retain completed observations even if interrupted in the middle of a pair.
        json_write(directory / "requests.json", reports)
        json_write(directory / "summary.json", summary)
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    print(json.dumps(summary, ensure_ascii=False))
    return 0 if summary["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
