#!/usr/bin/env python3
"""Validate and atomically import one exact retail-demo dataset version into ClickHouse."""

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid


REQUIRED_SPAN_FIELDS = {
    "Timestamp", "TraceId", "SpanId", "ParentSpanId", "SpanName", "TenantId", "AgentName",
    "Environment", "SessionId", "ScenarioId", "DatasetVersion", "RunnerVersion",
    "PromptVersion", "PolicyVersion", "Input", "Output", "ToolName", "ToolArguments",
    "ToolResult", "StatusCode", "DurationMs", "InputTokens", "OutputTokens", "ResponseId",
}
EVALUATOR_ONLY_FIELDS = {"expected_family", "observed_family", "matches_expected"}


def read_jsonl(path):
    rows = []
    with path.open() as source:
        for number, line in enumerate(source, 1):
            if line.strip():
                try:
                    rows.append(json.loads(line))
                except json.JSONDecodeError as error:
                    raise ValueError("%s:%d: %s" % (path, number, error))
    return rows


def content_hash(paths):
    digest = hashlib.sha256()
    for path in paths:
        digest.update(path.name.encode())
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def validate(manifest, spans, evaluations, allow_partial=False):
    version = manifest.get("dataset_version", "")
    if not version:
        raise ValueError("manifest dataset_version is required")
    if not spans or not evaluations:
        raise ValueError("spans and evaluations must both be non-empty")
    for span in spans:
        missing = REQUIRED_SPAN_FIELDS - set(span)
        if missing:
            raise ValueError("span rows are missing fields: " + ", ".join(sorted(missing)))
        if span["DatasetVersion"] != version:
            raise ValueError("span dataset version does not match manifest")
        if EVALUATOR_ONLY_FIELDS & set(span):
            raise ValueError("evaluator fields must not be present in trace spans")
        if span["AgentName"] != "support-agent":
            raise ValueError("unexpected agent name in trace spans")
    trace_tenants = {}
    span_keys = set()
    for span in spans:
        key = (span["TraceId"], span["SpanId"])
        if key in span_keys:
            raise ValueError("duplicate trace/span key: %s/%s" % key)
        span_keys.add(key)
        tenants = trace_tenants.setdefault(span["TraceId"], set())
        tenants.add(span["TenantId"])
    mixed = [trace_id for trace_id, tenants in trace_tenants.items() if len(tenants) != 1]
    if mixed:
        raise ValueError("tenant identity changes inside trace: " + ", ".join(sorted(mixed)))
    evaluation_traces = {row["trace_id"] for row in evaluations}
    if evaluation_traces != set(trace_tenants):
        raise ValueError("evaluation trace ids do not exactly match span trace ids")
    if any(row.get("dataset_version") != version for row in evaluations):
        raise ValueError("evaluation dataset version does not match manifest")
    if manifest.get("trace_count") != len(trace_tenants):
        raise ValueError("manifest trace_count does not match artifacts")
    if not allow_partial:
        base_count = manifest.get("base_scenario_count", len(trace_tenants))
        variants = manifest.get("variants_per_scenario", 1)
        if not isinstance(base_count, int) or not 10 <= base_count <= 15:
            raise ValueError("dataset must contain 10–15 base scenarios")
        if not isinstance(variants, int) or variants < 1 or base_count * variants != len(trace_tenants):
            raise ValueError("manifest variation counts do not match the trace artifacts")
    return version, len(trace_tenants)


def evaluation_row(row):
    return {
        "DatasetVersion": row["dataset_version"], "SessionId": row["session_id"],
        "ScenarioId": row["scenario_id"], "TraceId": row["trace_id"],
        "ExpectedFamily": row["expected_family"], "ObservedFamily": row["observed_family"],
        "MatchesExpected": row["matches_expected"],
        "ClaimedRefundSuccess": row["claimed_refund_success"],
        "RefundStatuses": row["refund_statuses"],
        "SettlementTimingExplained": row["settlement_timing_explained"],
        "PolicyVariant": row["policy_variant"],
    }


class ClickHouse:
    def __init__(self, url, username, password, database="default"):
        self.url = url.rstrip("/")
        self.database = database
        token = base64.b64encode((username + ":" + password).encode()).decode()
        self.headers = {"Authorization": "Basic " + token}

    @classmethod
    def from_dsn(cls, dsn):
        parsed = urllib.parse.urlparse(dsn)
        if parsed.scheme not in {"http", "https"} or not parsed.hostname:
            raise ValueError("CLICKHOUSE_DSN must use the HTTP or HTTPS interface")
        port = parsed.port or (443 if parsed.scheme == "https" else 80)
        url = "%s://%s:%d" % (parsed.scheme, parsed.hostname, port)
        return cls(url, urllib.parse.unquote(parsed.username or ""),
                   urllib.parse.unquote(parsed.password or ""), parsed.path.lstrip("/") or "default")

    def query(self, sql, data=None):
        url = self.url + "/?" + urllib.parse.urlencode({
            "database": self.database, "date_time_input_format": "best_effort", "query": sql,
        })
        request = urllib.request.Request(url, data=data or b"", method="POST", headers=self.headers)
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                return response.read().decode()
        except urllib.error.HTTPError as error:
            detail = error.read().decode(errors="replace")
            raise RuntimeError("ClickHouse returned HTTP %d: %s" % (error.code, detail))

    def insert_json_each_row(self, table, rows):
        payload = "".join(json.dumps(row, ensure_ascii=False, separators=(",", ":")) + "\n"
                          for row in rows).encode()
        self.query("INSERT INTO %s FORMAT JSONEachRow" % table, payload)


def import_dataset(database, manifest, spans, evaluations, digest, replace_version=None):
    version = manifest["dataset_version"]
    quoted_version = version.replace("'", "''")
    existing_raw = database.query(
        "SELECT ContentHash FROM lightship_demo.dataset_imports FINAL "
        "WHERE DatasetVersion = '%s' FORMAT JSONEachRow" % quoted_version)
    existing = [json.loads(line) for line in existing_raw.splitlines() if line.strip()]
    if existing and existing[-1]["ContentHash"] == digest:
        return "already imported"
    if existing and replace_version != version:
        raise RuntimeError("dataset version %s already exists with different content; "
                           "pass --replace-version %s to replace only that version" %
                           (version, version))

    suffix = uuid.uuid4().hex
    span_stage = "lightship_demo.agent_traces_stage_" + suffix
    eval_stage = "lightship_demo.evaluations_stage_" + suffix
    try:
        database.query("CREATE TABLE %s AS lightship_demo.agent_traces" % span_stage)
        database.query("CREATE TABLE %s AS lightship_demo.evaluations" % eval_stage)
        database.insert_json_each_row(span_stage, spans)
        database.insert_json_each_row(eval_stage, [evaluation_row(row) for row in evaluations])
        database.query("ALTER TABLE lightship_demo.agent_traces REPLACE PARTITION '%s' FROM %s" %
                       (quoted_version, span_stage))
        database.query("ALTER TABLE lightship_demo.evaluations REPLACE PARTITION '%s' FROM %s" %
                       (quoted_version, eval_stage))
        registry = {
            "DatasetVersion": version, "ContentHash": digest,
            "ImportedAt": manifest["completed_at"], "TraceCount": manifest["trace_count"],
            "SpanCount": len(spans), "Manifest": json.dumps(manifest, separators=(",", ":")),
        }
        database.insert_json_each_row("lightship_demo.dataset_imports", [registry])
    finally:
        database.query("DROP TABLE IF EXISTS %s" % span_stage)
        database.query("DROP TABLE IF EXISTS %s" % eval_stage)
    return "imported"


def main(argv=None, database=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("dataset", type=Path, help="directory containing manifest and JSONL files")
    parser.add_argument("--replace-version", help="exact dataset version allowed to be replaced")
    parser.add_argument("--allow-partial", action="store_true",
                        help="allow a non-showcase trace count for development")
    args = parser.parse_args(argv)
    manifest_path = args.dataset / "manifest.json"
    spans_path = args.dataset / "spans.jsonl"
    evaluations_path = args.dataset / "evaluations.jsonl"
    manifest = json.loads(manifest_path.read_text())
    spans = read_jsonl(spans_path)
    evaluations = read_jsonl(evaluations_path)
    version, trace_count = validate(manifest, spans, evaluations, args.allow_partial)
    if args.replace_version and args.replace_version != version:
        parser.error("--replace-version must exactly match %s" % version)
    digest = content_hash([manifest_path, spans_path, evaluations_path])
    if database is None:
        dsn = os.environ.get("CLICKHOUSE_DSN")
        if dsn:
            database = ClickHouse.from_dsn(dsn)
        else:
            url = os.environ.get("LIGHTSHIP_DEMO_CLICKHOUSE_URL")
            username = os.environ.get("LIGHTSHIP_DEMO_CLICKHOUSE_USER")
            password = os.environ.get("LIGHTSHIP_DEMO_CLICKHOUSE_PASSWORD")
            name = os.environ.get("LIGHTSHIP_DEMO_CLICKHOUSE_DATABASE", "default")
            if not all([url, username, password]):
                parser.error("CLICKHOUSE_DSN or LIGHTSHIP_DEMO_CLICKHOUSE_URL, USER, and PASSWORD are required")
            database = ClickHouse(url, username, password, name)
    result = import_dataset(database, manifest, spans, evaluations, digest, args.replace_version)
    print("%s %s (%d traces)" % (result, version, trace_count))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, RuntimeError, ValueError) as error:
        print("import failed: %s" % error, file=sys.stderr)
        sys.exit(1)
