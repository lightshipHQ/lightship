"""Validation and import-safety tests for the retail demo loader."""

import unittest

import load


def span(trace_id="trace-1", span_id="span-1", tenant="cedar", version="pilot-v1"):
    return {
        "Timestamp": "2026-09-08T12:00:00Z", "TraceId": trace_id, "SpanId": span_id,
        "ParentSpanId": "", "SpanName": "support-agent", "TenantId": tenant,
        "AgentName": "support-agent", "Environment": "controlled-demo",
        "SessionId": "session-1", "ScenarioId": "scenario-1", "DatasetVersion": version,
        "RunnerVersion": "runner-v1", "PromptVersion": "prompt-v1", "PolicyVersion": "current",
        "Input": "question", "Output": "answer", "ToolName": "", "ToolArguments": "",
        "ToolResult": "", "StatusCode": "OK", "DurationMs": 1,
        "InputTokens": 2, "OutputTokens": 3, "ResponseId": "response-1",
    }


def evaluation(trace_id="trace-1", version="pilot-v1"):
    return {
        "dataset_version": version, "session_id": "session-1", "scenario_id": "scenario-1",
        "trace_id": trace_id, "expected_family": "ordinary_success",
        "observed_family": "ordinary_success", "matches_expected": True,
        "claimed_refund_success": True, "refund_statuses": ["completed"],
        "settlement_timing_explained": True, "policy_variant": "current",
    }


class FakeClickHouse:
    def __init__(self, existing=""):
        self.existing = existing
        self.queries = []
        self.inserts = []

    def query(self, sql, data=None):
        self.queries.append(sql)
        if sql.startswith("SELECT ContentHash"):
            return self.existing
        return ""

    def insert_json_each_row(self, table, rows):
        self.inserts.append((table, rows))


class LoaderTests(unittest.TestCase):
    def setUp(self):
        self.manifest = {
            "dataset_version": "pilot-v1", "trace_count": 1,
            "completed_at": "2026-09-08T12:01:00Z",
        }

    def test_validation_rejects_tenant_changes_inside_trace(self):
        spans = [span(), span(span_id="span-2", tenant="northstar")]
        with self.assertRaisesRegex(ValueError, "tenant identity changes"):
            load.validate(self.manifest, spans, [evaluation()], allow_partial=True)

    def test_validation_rejects_evaluator_label_leakage(self):
        leaking = span()
        leaking["observed_family"] = "ordinary_success"
        with self.assertRaisesRegex(ValueError, "evaluator fields"):
            load.validate(self.manifest, [leaking], [evaluation()], allow_partial=True)

    def test_identical_import_is_a_noop(self):
        database = FakeClickHouse('{"ContentHash":"same"}\n')
        result = load.import_dataset(database, self.manifest, [span()], [evaluation()], "same")
        self.assertEqual(result, "already imported")
        self.assertEqual(database.inserts, [])

    def test_conflicting_version_requires_exact_replace_flag(self):
        database = FakeClickHouse('{"ContentHash":"old"}\n')
        with self.assertRaisesRegex(RuntimeError, "--replace-version pilot-v1"):
            load.import_dataset(database, self.manifest, [span()], [evaluation()], "new")
        self.assertFalse(any("REPLACE PARTITION" in query for query in database.queries))

    def test_replace_stages_both_artifacts_and_registers_last(self):
        database = FakeClickHouse('{"ContentHash":"old"}\n')
        result = load.import_dataset(database, self.manifest, [span()], [evaluation()], "new",
                                     replace_version="pilot-v1")
        self.assertEqual(result, "imported")
        self.assertEqual(len(database.inserts), 3)
        self.assertTrue(database.inserts[0][0].startswith("lightship_demo.agent_traces_stage_"))
        self.assertTrue(database.inserts[1][0].startswith("lightship_demo.evaluations_stage_"))
        self.assertEqual(database.inserts[2][0], "lightship_demo.dataset_imports")
        replacements = [query for query in database.queries if "REPLACE PARTITION" in query]
        self.assertEqual(len(replacements), 2)
        self.assertLess(database.queries.index(replacements[0]),
                        next(index for index, item in enumerate(database.queries)
                             if item.startswith("DROP TABLE")))

    def test_evaluation_ids_must_match_loaded_traces(self):
        with self.assertRaisesRegex(ValueError, "do not exactly match"):
            load.validate(self.manifest, [span()], [evaluation("trace-2")], allow_partial=True)

    def test_showcase_variation_counts_are_accepted(self):
        spans = [span("trace-%d" % index, version="demo-v1") for index in range(120)]
        evaluations = [evaluation("trace-%d" % index, version="demo-v1")
                       for index in range(120)]
        manifest = dict(self.manifest, dataset_version="demo-v1", trace_count=120,
                        base_scenario_count=12, variants_per_scenario=10)
        self.assertEqual(load.validate(manifest, spans, evaluations), ("demo-v1", 120))


if __name__ == "__main__":
    unittest.main()
