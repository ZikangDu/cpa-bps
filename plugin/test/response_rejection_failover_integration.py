#!/usr/bin/env python3
"""Verify account failover after HTTP, SSE and HTTP-200 JSON quota rejection."""

import argparse
import hashlib
import http.client
import json
import pathlib
import time
from contextlib import closing

import quota_failover_integration as quota
from integration import require


class Fixture(quota.Fixture):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        payload = json.loads(self.rfile.read(length))
        account = self.account()
        request_id = quota.MARKER.findall(json.dumps(payload.get("input")))[-1]
        with self.server.lock:
            self.server.generations.append({"request": request_id, "account": account, "model": payload["model"]})
            exhausted = self.server.accounts[account]["remaining"] == 0
        if exhausted:
            error = {"code": "rate_limit_exceeded", "message": "PRIVATE fixture quota detail"}
            wire = self.server.rejection_wire
            if wire == "http":
                self.reply(429, {"error": error})
            elif wire == "json":
                self.reply(200, {"error": error})
            else:
                body = ('data: '+json.dumps({"type": "response.created", "response": {"id": "r", "status": "in_progress"}})+'\n\n').encode()
                body += ('data: '+json.dumps({"type": "error", **error})+'\n\n').encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Connection", "close")
                self.end_headers(); self.wfile.write(body)
            return
        self.reply(200, {"id": "resp_"+request_id, "status": "completed", "model": payload["model"],
                         "output": [{"type": "message", "role": "assistant", "content": [
                             {"type": "output_text", "text": "ACK "+request_id}]}]})


class Suite(quota.Suite):
    wire = "sse"
    stream = True

    def run(self):
        self.fixture.rejection_wire = self.wire
        self.set_quota(quota.TOKENS, 1, time.time()+3600)
        session = "fixture-error-followup"
        warm = self.request("warm", session=session)
        require(warm["http"] == 200, "warm-up failed")
        owner = warm["upstream_accounts"][0]
        other = next(account for account in quota.TOKENS if account != owner)
        self.set_quota([owner], 0, time.time()+120)
        payload = {"model": next(iter(quota.MODELS)), "input": "FIXTURE_QUOTA_reject", "stream": self.stream,
                   "store": False, "prompt_cache_key": session}
        with closing(http.client.HTTPConnection(quota.LOOPBACK, self.port, timeout=12)) as client:
            client.request("POST", "/v1/responses", json.dumps(payload), {"Content-Type": "application/json", "Authorization": "Bearer "+quota.API_KEY})
            response = client.getresponse(); body = response.read(quota.MAX_BODY+1)
            status, content_type = response.status, response.getheader("Content-Type", "")
        generations, reads = self.snapshots()
        first_attempts = [g["account"] for g in generations if g["request"] == "reject"]
        require(b"PRIVATE" not in body, "private upstream text escaped")
        if self.wire == "http":
            require(status == 200 and first_attempts == [owner, other], "explicit HTTP429 failed to fail over once")
        else:
            require(first_attempts == [owner], "HTTP200 terminal rejection was replayed")
            if self.wire == "sse" and self.stream:
                require(status == 200 and "text/event-stream" in content_type, "established SSE status was replaced")
                require(b"bps_rate_limited" in body and b"response.completed" not in body, "terminal SSE rejection became success")
            else:
                require(status == 502 and b"bps_rate_limited" in body, "HTTP200 error body became success or lost machine code")
        followup = self.request("followup", session=session)
        require(followup["http"] == 200 and followup["upstream_accounts"] == [other], "next request did not select healthy account")
        again = self.request("followup_again", session=session)
        require(again["http"] == 200 and again["upstream_accounts"] == [other], "replacement affinity was lost")
        generations, reads = self.snapshots()
        expected = 5 if self.wire == "http" else 4
        require(len(generations) == expected, "unexpected generation count or replay")
        require(reads.count(owner) == 2 and reads.count(other) == 1, "quota admission was not refreshed/coalesced")
        return {"passed": True, "wire": self.wire, "client_stream": self.stream,
                "initial_http_status": status, "initial_upstream_accounts": first_attempts,
                "next_account": other, "sticky_replacement": True, "upstream_generations": len(generations),
                "quota_reads": reads, "requests": self.reports}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=pathlib.Path, required=True)
    parser.add_argument("--plugin", type=pathlib.Path, required=True)
    parser.add_argument("--evidence", type=pathlib.Path, required=True)
    args = parser.parse_args()
    cpa, plugin = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    if args.evidence.exists(): parser.error("evidence already exists")
    runs = []
    for wire, stream in [("http", False), ("sse", True), ("sse", False), ("json", True), ("json", False)]:
        scenario = type("RejectionScenario", (Suite,), {"wire": wire, "stream": stream})
        runs.append(quota.run(cpa, plugin, fixture_type=Fixture, suite_type=scenario))
    result = {"passed": all(r["passed"] for r in runs), "synthetic_loopback_only": True, "runs": runs,
              "cpa_sha256": hashlib.sha256(cpa.read_bytes()).hexdigest(), "plugin_sha256": hashlib.sha256(plugin.read_bytes()).hexdigest()}
    args.evidence.parent.mkdir(parents=True, exist_ok=True)
    args.evidence.write_text(json.dumps(result, indent=2)+"\n")
    print(json.dumps(result))


if __name__ == "__main__":
    main()
