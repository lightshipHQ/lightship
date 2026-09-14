#!/usr/bin/env python3
"""Configure and verify the hosted Railway retail demo after importing a dataset."""

import argparse
import http.cookiejar
import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone


BASE_URL = "https://lightship-production.up.railway.app"


class Client:
    def __init__(self, base_url=BASE_URL):
        self.base_url = base_url.rstrip("/")
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def request(self, method, path, body=None, expected=200, preview_role=None):
        headers = {"Content-Type": "application/json"}
        if preview_role:
            headers["X-LightShip-Preview-Role"] = preview_role
        request = urllib.request.Request(
            self.base_url + path, data=None if body is None else json.dumps(body).encode(),
            method=method, headers=headers)
        try:
            with self.opener.open(request, timeout=20) as response:
                status, payload = response.status, response.read()
        except urllib.error.HTTPError as error:
            status, payload = error.code, error.read()
        if status != expected:
            raise RuntimeError("%s %s: expected %d, got %d: %s" %
                               (method, path, expected, status, payload.decode()))
        return json.loads(payload)

    def login(self, username, password):
        self.request("POST", "/login", {"username": username, "password": password})


def configure(admin):
    admin.request("PUT", "/schema/binding", {
        "table": "lightship_demo.agent_traces", "trace_id": "TraceId",
        "timestamp": "Timestamp", "span_id": "SpanId", "parent_span_id": "ParentSpanId",
        "name": "SpanName",
    })
    admin.request("PUT", "/schema/fields", {"fields": [
        {"name": "TraceId", "logical_type": "string", "filterable": True, "policy": False},
        {"name": "TenantId", "logical_type": "string", "filterable": True, "policy": True},
        {"name": "AgentName", "logical_type": "string", "filterable": True, "policy": True},
        {"name": "Environment", "logical_type": "string", "filterable": True, "policy": False},
        {"name": "SessionId", "logical_type": "string", "filterable": True, "policy": False},
        {"name": "DatasetVersion", "logical_type": "string", "filterable": True, "policy": False},
        {"name": "ScenarioId", "logical_type": "string", "filterable": True, "policy": False},
        {"name": "StatusCode", "logical_type": "string", "filterable": True, "policy": False},
    ]})
    for role, title, expression in [
        ("cedar-account-manager", "Cedar support agent traces",
         'TenantId == "cedar" && AgentName == "support-agent"'),
        ("northstar-account-manager", "Northstar support agent traces",
         'TenantId == "northstar" && AgentName == "support-agent"'),
        ("support-operations", "All support agent traces", 'AgentName == "support-agent"'),
    ]:
        admin.request("PUT", "/roles/" + role, {"policies": [{
            "title": title, "expression": expression,
        }]})


def paged_trace_ids(client, window, limit=3, preview_role=None):
    ids, cursor, seen_cursors = [], None, set()
    while True:
        request = {"from": window["from"], "to": window["to"], "limit": limit}
        if cursor:
            request["cursor"] = cursor
        body = client.request("POST", "/traces/query", request, preview_role=preview_role)
        for span in body["spans"]:
            if span["TraceId"] not in ids:
                ids.append(span["TraceId"])
        cursor = body.get("next_cursor")
        if not cursor:
            return ids
        if cursor in seen_cursors:
            raise RuntimeError("pagination returned a repeated cursor")
        seen_cursors.add(cursor)


def verify(manifest, evaluations, base_url, admin_password):
    minimum = manifest["minimum_timestamp"]
    maximum = manifest["maximum_timestamp"]
    upper = datetime.fromisoformat(maximum.replace("Z", "+00:00")) + timedelta(seconds=1)
    window = {"from": minimum, "to": upper.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")}
    admin = Client(base_url)
    admin.login("admin", admin_password)
    all_ids = set(paged_trace_ids(admin, window))
    cedar_ids = set(paged_trace_ids(admin, window, preview_role="cedar-account-manager"))
    northstar_ids = set(paged_trace_ids(
        admin, window, preview_role="northstar-account-manager"))
    operations_ids = set(paged_trace_ids(admin, window, preview_role="support-operations"))
    if len(all_ids) != manifest["trace_count"]:
        raise RuntimeError("admin trace count does not match manifest")
    evaluation_by_trace = {row["trace_id"]: row for row in evaluations}
    if not cedar_ids or any(evaluation_by_trace[trace_id]["scenario_id"].startswith("northstar-")
                            for trace_id in cedar_ids):
        raise RuntimeError("Cedar account manager tenant isolation failed")
    if not northstar_ids or any(not evaluation_by_trace[trace_id]["scenario_id"].startswith(
            "northstar-") for trace_id in northstar_ids):
        raise RuntimeError("Northstar account manager tenant isolation failed")
    if cedar_ids & northstar_ids or cedar_ids | northstar_ids != all_ids:
        raise RuntimeError("tenant preview roles do not partition the dataset")
    if operations_ids != all_ids:
        raise RuntimeError("support operations cannot reach all support-agent traces")
    northstar_id = next(trace_id for trace_id in all_ids - cedar_ids)
    denied = admin.request("GET", "/traces/" + northstar_id, expected=404,
                           preview_role="cedar-account-manager")
    missing = admin.request("GET", "/traces/not-a-real-trace", expected=404,
                            preview_role="cedar-account-manager")
    if denied != missing:
        raise RuntimeError("denied and missing trace responses differ")
    observed = {row["observed_family"] for row in evaluations}
    required = {"backend_timeout", "outdated_return_policy", "needs_review", "ordinary_success"}
    if missing_families := required - observed:
        raise RuntimeError("pilot is missing observed evidence families: " +
                           ", ".join(sorted(missing_families)))
    timeout = next(row for row in evaluations if row["observed_family"] == "backend_timeout")
    evidence = admin.request("GET", "/traces/" + timeout["trace_id"],
                             preview_role="cedar-account-manager")
    timed_out = any(span.get("ToolResult") and
                    json.loads(span["ToolResult"]).get("status") == "timeout"
                    for span in evidence["spans"])
    if not timed_out:
        raise RuntimeError("backend-timeout trace lacks the recorded timeout tool result")
    print("PASS all %d pilot traces are reachable to admin" % len(all_ids))
    print("PASS Cedar sees %d traces and no Northstar traces" % len(cedar_ids))
    print("PASS Northstar sees %d traces and support operations sees all %d" %
          (len(northstar_ids), len(operations_ids)))
    print("PASS pagination, denied detail, and observed pilot evidence")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("dataset", type=Path)
    parser.add_argument("--base-url", default=os.environ.get("LIGHTSHIP_DEMO_BASE_URL", BASE_URL))
    args = parser.parse_args(argv)
    admin_password = os.environ.get("LIGHTSHIP_DEMO_ADMIN_PASSWORD", "")
    if not admin_password:
        parser.error("LIGHTSHIP_DEMO_ADMIN_PASSWORD is required")
    manifest = json.loads((args.dataset / "manifest.json").read_text())
    evaluations = [json.loads(line) for line in
                   (args.dataset / "evaluations.jsonl").read_text().splitlines() if line.strip()]
    admin = Client(args.base_url)
    admin.login("admin", admin_password)
    configure(admin)
    verify(manifest, evaluations, args.base_url, admin_password)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, RuntimeError, ValueError) as error:
        print("retail demo verification failed: %s" % error, file=sys.stderr)
        sys.exit(1)
