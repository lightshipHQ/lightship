"""Contract tests for the controlled retail pilot runner."""

import json
import io
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import runner


class FakeResponsesClient:
    def __init__(self, responses):
        self.responses = list(responses)
        self.payloads = []

    def create(self, payload):
        self.payloads.append(payload)
        return self.responses.pop(0)


def function_response(number, name, arguments):
    return {
        "id": "resp-%d" % number,
        "output": [{"type": "function_call", "call_id": "call-%d" % number,
                    "name": name, "arguments": json.dumps(arguments)}],
        "usage": {"input_tokens": 10, "output_tokens": 5},
    }


class RunnerTests(unittest.TestCase):
    def setUp(self):
        self.spec = json.loads(runner.DEFAULT_SCENARIOS.read_text())
        self.scenario = next(item for item in self.spec["scenarios"]
                             if item["id"] == "cedar-success-01")
        self.run = {
            "session_id": "pilot-test", "dataset_version": self.spec["dataset_version"],
            "prompt_version": self.spec["prompt_version"],
            "runner_version": self.spec["runner_version"],
        }

    def test_scenario_set_covers_both_tenants_and_every_family(self):
        scenarios = self.spec["scenarios"]
        self.assertGreaterEqual(len(scenarios), 10)
        self.assertLessEqual(len(scenarios), 15)
        self.assertEqual({item["tenant"] for item in scenarios}, {"cedar", "northstar"})
        self.assertEqual({item["expected_family"] for item in scenarios}, {
            "refund_promised_after_rejection", "outdated_return_policy",
            "refund_succeeded_customer_returns", "backend_timeout", "correct_refusal",
            "ordinary_success",
        })

    def test_variants_add_distinct_orders_messages_items_and_prices(self):
        scenarios = runner.expand_scenarios(self.spec["scenarios"], 3)
        self.assertEqual(len(scenarios), 36)
        self.assertEqual(len({item["id"] for item in scenarios}), 36)
        self.assertEqual(len({item["order_id"] for item in scenarios}), 36)
        copies = [item for item in scenarios if item["id"].startswith("cedar-success-01")]
        self.assertEqual(len(copies), 3)
        self.assertEqual(len({item["customer_message"] for item in copies}), 3)
        self.assertEqual(len({item["item"] for item in copies}), 3)
        self.assertEqual(len({item["order_total_usd"] for item in copies}), 3)
        self.assertEqual({item["refund_outcome"] for item in copies}, {"completed"})

    def test_real_agent_loop_records_tools_and_keeps_evaluation_separate(self):
        client = FakeResponsesClient([
            function_response(1, "lookup_order", {"order_id": "CED-1008"}),
            function_response(2, "get_return_policy", {"retailer": "Cedar & Co."}),
            function_response(3, "check_return_eligibility", {"order_id": "CED-1008"}),
            function_response(4, "issue_refund", {"order_id": "CED-1008"}),
            {
                "id": "resp-5", "output": [{"type": "message", "content": [
                    {"type": "output_text", "text":
                     "Your refund was processed and should appear in 5–7 business days."}
                ]}], "usage": {"input_tokens": 12, "output_tokens": 8},
            },
        ])
        spans, evaluation = runner.run_scenario(client, runner.DEFAULT_MODEL, self.run, self.scenario)
        self.assertEqual(spans[0]["SpanName"], "support-agent")
        self.assertEqual(spans[0]["TenantId"], "cedar")
        self.assertEqual(spans[0]["InputTokens"], 52)
        self.assertEqual([span["ToolName"] for span in spans if span["ToolName"]],
                         ["lookup_order", "get_return_policy", "check_return_eligibility",
                          "issue_refund"])
        self.assertEqual(evaluation["observed_family"], "ordinary_success")
        serialized_spans = json.dumps(spans)
        self.assertNotIn("expected_family", serialized_spans)
        self.assertNotIn("observed_family", serialized_spans)
        self.assertTrue(all(payload["store"] is False for payload in client.payloads))
        self.assertTrue(any(item.get("type") == "function_call_output"
                            for item in client.payloads[-1]["input"]))

    def test_anthropic_adapter_normalizes_tool_use_without_changing_trace_loop(self):
        api_response = io.BytesIO(json.dumps({
            "id": "msg-1", "model": "claude-sonnet-4-6",
            "content": [{"type": "text", "text": "I'll check."}, {
                "type": "tool_use", "id": "toolu-1", "name": "lookup_order",
                "input": {"order_id": "CED-1008"},
            }],
            "usage": {"input_tokens": 11, "output_tokens": 7},
        }).encode())
        api_response.__enter__ = lambda value: value
        api_response.__exit__ = lambda *unused: None
        client = runner.AnthropicClient("not-a-real-key")
        payload = {
            "model": runner.ANTHROPIC_MODEL, "instructions": runner.INSTRUCTIONS,
            "input": [{"role": "user", "content": "hello"}], "tools": runner.TOOLS,
        }
        with mock.patch("urllib.request.urlopen", return_value=api_response) as urlopen:
            response = client.create(payload)
        sent = json.loads(urlopen.call_args.args[0].data)
        self.assertEqual(sent["tools"][0]["input_schema"], runner.TOOLS[0]["parameters"])
        self.assertEqual(sent["tool_choice"]["disable_parallel_tool_use"], True)
        self.assertEqual(response["output"][1], {
            "type": "function_call", "call_id": "toolu-1", "name": "lookup_order",
            "arguments": '{"order_id":"CED-1008"}',
        })
        follow_up = runner.AnthropicClient.messages([
            {"role": "user", "content": "hello"}, *response["output"],
            {"type": "function_call_output", "call_id": "toolu-1",
             "output": '{"status":"found"}'},
        ])
        self.assertEqual(follow_up[-2]["role"], "assistant")
        self.assertEqual(follow_up[-1]["content"][0]["type"], "tool_result")

    def test_claude_cli_adapter_disables_tools_and_normalizes_one_action(self):
        envelope = {
            "uuid": "cli-response-1", "structured_output": {
                "kind": "tool", "name": "lookup_order",
                "arguments": {"order_id": "CED-1008"}, "response": "",
            },
            "usage": {"input_tokens": 13, "output_tokens": 6},
        }
        completed = mock.Mock(returncode=0, stdout=json.dumps(envelope), stderr="")
        client = runner.ClaudeCLIClient(executable="/fake/claude")
        payload = {
            "model": runner.CLAUDE_CLI_MODEL, "instructions": runner.INSTRUCTIONS,
            "input": [{"role": "user", "content": "hello"}], "tools": runner.TOOLS,
        }
        with mock.patch("subprocess.run", return_value=completed) as run:
            response = client.create(payload)
        command = run.call_args.args[0]
        self.assertIn("--safe-mode", command)
        self.assertEqual(command[command.index("--tools") + 1], "")
        self.assertIn("--no-session-persistence", command)
        self.assertEqual(response["output"][0]["name"], "lookup_order")
        self.assertEqual(response["usage"], {"input_tokens": 13, "output_tokens": 6})

    def test_main_writes_manifest_and_separate_jsonl_artifacts(self):
        client = FakeResponsesClient([{
            "id": "resp-final", "output": [{"type": "message", "content": [
                {"type": "output_text", "text": "I need to escalate this case."}
            ]}], "usage": {"input_tokens": 3, "output_tokens": 4},
        }])
        with tempfile.TemporaryDirectory() as directory:
            result = runner.main([
                "--output-dir", directory, "--only", "cedar-rejected-01",
            ], client=client)
            self.assertEqual(result, 0)
            output = Path(directory)
            manifest = json.loads((output / "manifest.json").read_text())
            self.assertEqual(manifest["trace_count"], 1)
            spans = [json.loads(line) for line in (output / "spans.jsonl").read_text().splitlines()]
            evaluations = [json.loads(line) for line in
                           (output / "evaluations.jsonl").read_text().splitlines()]
            self.assertEqual(len({span["TraceId"] for span in spans}), 1)
            self.assertEqual(len(evaluations), 1)
            self.assertNotIn("expected_family", spans[0])

    def test_refund_rejection_is_measured_from_execution_not_expected_label(self):
        scenario = dict(self.scenario, expected_family="ordinary_success", refund_outcome="rejected")
        world = runner.RetailWorld(scenario)
        world.execute("issue_refund", {"order_id": scenario["order_id"]})
        evaluation = runner.evaluate(scenario, world, "Your refund has been processed.")
        self.assertEqual(evaluation["observed_family"], "refund_promised_after_rejection")
        self.assertFalse(evaluation["matches_expected"])

    def test_conditional_future_refund_is_not_measured_as_completed(self):
        scenario = dict(self.scenario, refund_outcome="rejected")
        world = runner.RetailWorld(scenario)
        world.execute("issue_refund", {"order_id": scenario["order_id"]})
        evaluation = runner.evaluate(
            scenario, world, "You will receive confirmation once the refund has been issued.")
        self.assertFalse(evaluation["claimed_refund_success"])

    def test_existing_run_artifacts_are_never_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            (output / "manifest.json").write_text("keep me")
            with self.assertRaises(SystemExit):
                runner.main(["--output-dir", directory], client=FakeResponsesClient([]))
            self.assertEqual((output / "manifest.json").read_text(), "keep me")


if __name__ == "__main__":
    unittest.main()
