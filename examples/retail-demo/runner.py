#!/usr/bin/env python3
"""Run the controlled retail-support pilot through one Responses API agent."""

import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timezone


DIRECTORY = Path(__file__).resolve().parent
DEFAULT_SCENARIOS = DIRECTORY / "scenarios.json"
DEFAULT_OUTPUT = DIRECTORY.parent.parent / "local" / "retail-demo"
DEFAULT_MODEL = "gpt-5.4-mini-2026-03-17"
API_URL = "https://api.openai.com/v1/responses"
ANTHROPIC_MODEL = "claude-sonnet-4-6"
ANTHROPIC_API_URL = "https://api.anthropic.com/v1/messages"
CLAUDE_CLI_MODEL = "claude-sonnet-4-6"
MAX_AGENT_TURNS = 8

VARIANT_CONTEXT = [
    "",
    "Please verify the order record before answering because the status has not changed.",
    "I would like a clear explanation of what happens next.",
    "This is my second contact, so please check the latest backend status.",
    "Please distinguish what is confirmed from what is still pending.",
    "I need an update I can rely on before contacting my bank.",
    "Could you check both the return policy and the actual order state?",
    "Please tell me whether anyone needs to review this manually.",
    "I am travelling soon and need the most accurate status available.",
    "Please do not estimate—check the recorded return and refund state.",
]

TENANT_ITEMS = {
    "cedar": ["jacket", "boots", "trainers", "sweater", "handbag"],
    "northstar": ["tent", "sleeping bag", "rain shell", "backpack", "camp stove"],
}

INSTRUCTIONS = """You are the support agent for the retailer named in the customer's message.
Investigate before answering. Use lookup_order and get_return_policy, then check_return_eligibility
before attempting a refund. Call issue_refund only when eligibility says eligible. Use the tool
results to resolve the customer's request, and offer escalate_to_human when you cannot complete it.
Be concise and do not mention internal scenario or evaluation labels."""

TOOLS = [
    {
        "type": "function", "name": "lookup_order", "strict": True,
        "description": "Look up the current retailer order state.",
        "parameters": {
            "type": "object", "properties": {"order_id": {"type": "string"}},
            "required": ["order_id"], "additionalProperties": False,
        },
    },
    {
        "type": "function", "name": "get_return_policy", "strict": True,
        "description": "Retrieve the return policy available to the support agent.",
        "parameters": {
            "type": "object", "properties": {"retailer": {"type": "string"}},
            "required": ["retailer"], "additionalProperties": False,
        },
    },
    {
        "type": "function", "name": "check_return_eligibility", "strict": True,
        "description": "Check refund eligibility using the order and current backend rules.",
        "parameters": {
            "type": "object", "properties": {"order_id": {"type": "string"}},
            "required": ["order_id"], "additionalProperties": False,
        },
    },
    {
        "type": "function", "name": "issue_refund", "strict": True,
        "description": "Attempt a refund for an eligible, received return.",
        "parameters": {
            "type": "object", "properties": {"order_id": {"type": "string"}},
            "required": ["order_id"], "additionalProperties": False,
        },
    },
    {
        "type": "function", "name": "escalate_to_human", "strict": True,
        "description": "Send a case that needs manual review to a human support queue.",
        "parameters": {
            "type": "object",
            "properties": {
                "order_id": {"type": "string"}, "reason": {"type": "string"},
            },
            "required": ["order_id", "reason"], "additionalProperties": False,
        },
    },
]


def utc_now():
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


class ResponsesClient:
    provider = "openai"
    span_name = "openai.responses.create"

    def __init__(self, api_key, url=API_URL):
        self.api_key = api_key
        self.url = url

    def create(self, payload):
        request = urllib.request.Request(
            self.url, data=json.dumps(payload).encode(), method="POST",
            headers={"Authorization": "Bearer " + self.api_key,
                     "Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=90) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            detail = error.read().decode(errors="replace")
            raise RuntimeError("Responses API returned HTTP %d: %s" % (error.code, detail))


class AnthropicClient:
    provider = "anthropic"
    span_name = "anthropic.messages.create"

    def __init__(self, api_key, url=ANTHROPIC_API_URL):
        self.api_key = api_key
        self.url = url

    @staticmethod
    def messages(conversation):
        messages, assistant, tool_results = [], [], []

        def flush_assistant():
            if assistant:
                messages.append({"role": "assistant", "content": list(assistant)})
                assistant.clear()

        def flush_tools():
            if tool_results:
                messages.append({"role": "user", "content": list(tool_results)})
                tool_results.clear()

        for item in conversation:
            if item.get("role") == "user":
                flush_assistant()
                flush_tools()
                messages.append({"role": "user", "content": item["content"]})
            elif item.get("type") == "message":
                flush_tools()
                assistant.extend({"type": "text", "text": part.get("text", "")}
                                 for part in item.get("content", [])
                                 if part.get("type") == "output_text")
            elif item.get("type") == "function_call":
                flush_tools()
                assistant.append({
                    "type": "tool_use", "id": item["call_id"], "name": item["name"],
                    "input": json.loads(item.get("arguments", "{}")),
                })
            elif item.get("type") == "function_call_output":
                flush_assistant()
                tool_results.append({
                    "type": "tool_result", "tool_use_id": item["call_id"],
                    "content": item["output"],
                })
        flush_assistant()
        flush_tools()
        return messages

    def create(self, payload):
        tools = [{
            "name": tool["name"], "description": tool["description"],
            "input_schema": tool["parameters"],
        } for tool in payload["tools"]]
        body = {
            "model": payload["model"], "max_tokens": 1200,
            "system": payload["instructions"],
            "messages": self.messages(payload["input"]), "tools": tools,
            "tool_choice": {"type": "auto", "disable_parallel_tool_use": True},
        }
        request = urllib.request.Request(
            self.url, data=json.dumps(body).encode(), method="POST",
            headers={"x-api-key": self.api_key, "anthropic-version": "2023-06-01",
                     "Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=90) as response:
                result = json.load(response)
        except urllib.error.HTTPError as error:
            detail = error.read().decode(errors="replace")
            raise RuntimeError("Anthropic Messages API returned HTTP %d: %s" %
                               (error.code, detail))

        text_blocks = [{"type": "output_text", "text": block.get("text", "")}
                       for block in result.get("content", []) if block.get("type") == "text"]
        output = []
        if text_blocks:
            output.append({"type": "message", "content": text_blocks})
        output.extend({
            "type": "function_call", "call_id": block["id"], "name": block["name"],
            "arguments": json.dumps(block.get("input", {}), separators=(",", ":")),
        } for block in result.get("content", []) if block.get("type") == "tool_use")
        usage = result.get("usage") or {}
        return {
            "id": result.get("id", ""), "model": result.get("model", ""), "output": output,
            "usage": {"input_tokens": usage.get("input_tokens", 0),
                      "output_tokens": usage.get("output_tokens", 0)},
        }


class ClaudeCLIClient:
    provider = "claude-cli"
    span_name = "anthropic.claude_code.print"
    schema = {
        "type": "object",
        "properties": {
            "kind": {"type": "string", "enum": ["tool", "final"]},
            "name": {"type": "string"},
            "arguments": {"type": "object"},
            "response": {"type": "string"},
        },
        "required": ["kind", "name", "arguments", "response"],
        "additionalProperties": False,
    }

    def __init__(self, executable=None):
        self.executable = executable or shutil.which("claude")
        if not self.executable:
            raise RuntimeError("claude CLI is not installed")

    @staticmethod
    def transcript(conversation):
        lines = []
        for item in conversation:
            if item.get("role") == "user":
                lines.append("CUSTOMER\n" + item["content"])
            elif item.get("type") == "message":
                text = "\n".join(part.get("text", "") for part in item.get("content", [])
                                 if part.get("type") == "output_text")
                if text:
                    lines.append("ASSISTANT\n" + text)
            elif item.get("type") == "function_call":
                lines.append("ASSISTANT TOOL CALL\n%s %s" %
                             (item["name"], item.get("arguments", "{}")))
            elif item.get("type") == "function_call_output":
                lines.append("TOOL RESULT for %s\n%s" % (item["call_id"], item["output"]))
        return "\n\n".join(lines)

    def create(self, payload):
        tool_specs = [{"name": tool["name"], "description": tool["description"],
                       "parameters": tool["parameters"]} for tool in payload["tools"]]
        prompt = """Continue this controlled customer-support conversation.
You are choosing the next action for an external controller. Returning kind=tool will run that
tool, even though Claude Code's built-in tools are disabled. Follow the required workflow in the
system prompt: if a required step is missing, request its tool instead of claiming a technical
problem. A TOOL RESULT in the transcript is a completed call; use it and continue to the next
required step.

Return kind=tool to call exactly one available tool. Set name and arguments for that tool and set
response to an empty string. Return kind=final only when you can answer the customer; set response
to the complete customer-facing answer and set name to an empty string and arguments to an empty
object. Never invent a tool result.

AVAILABLE TOOLS
%s

CONVERSATION
%s""" % (json.dumps(tool_specs, separators=(",", ":")), self.transcript(payload["input"]))
        command = [
            self.executable, "-p", "--safe-mode", "--tools", "",
            "--permission-prompts", "none", "--strict-mcp-config",
            "--disable-slash-commands", "--no-chrome", "--no-session-persistence",
            "--model", payload["model"], "--output-format", "json",
            "--json-schema", json.dumps(self.schema, separators=(",", ":")),
            "--system-prompt", payload["instructions"],
        ]
        completed = subprocess.run(
            command, input=prompt, text=True, capture_output=True, timeout=120, check=False)
        if completed.returncode != 0:
            detail = (completed.stderr or completed.stdout).strip()
            raise RuntimeError("claude -p exited %d: %s" % (completed.returncode, detail))
        try:
            envelope = json.loads(completed.stdout)
            action = envelope.get("structured_output") or json.loads(envelope["result"])
        except (KeyError, TypeError, ValueError) as error:
            raise RuntimeError("claude -p returned malformed structured output: %s" % error)
        if action["kind"] == "tool":
            known = {tool["name"] for tool in payload["tools"]}
            if action["name"] not in known:
                raise RuntimeError("claude -p requested unknown tool %r" % action["name"])
            output = [{
                "type": "function_call", "call_id": "cli-" + uuid.uuid4().hex[:16],
                "name": action["name"],
                "arguments": json.dumps(action["arguments"], separators=(",", ":")),
            }]
        else:
            if not action["response"].strip():
                raise RuntimeError("claude -p returned an empty final response")
            output = [{"type": "message", "content": [
                {"type": "output_text", "text": action["response"].strip()},
            ]}]
        usage = envelope.get("usage") or {}
        return {
            "id": envelope.get("uuid") or envelope.get("session_id", ""), "output": output,
            "usage": {"input_tokens": usage.get("input_tokens", 0),
                      "output_tokens": usage.get("output_tokens", 0)},
        }


class RetailWorld:
    def __init__(self, scenario):
        self.scenario = scenario
        self.calls = []

    def execute(self, name, arguments):
        scenario = self.scenario
        order_id = arguments.get("order_id")
        if order_id and order_id != scenario["order_id"]:
            result = {"status": "not_found", "order_id": order_id}
        elif name == "lookup_order":
            result = {
                "status": "found", "order_id": scenario["order_id"],
                "retailer": scenario["retailer"], "order_age_days": scenario["order_age_days"],
                "return_received": scenario["return_received"],
                "item_condition": scenario["item_condition"],
                "item": scenario.get("item", "retail item"),
                "order_total_usd": scenario.get("order_total_usd", 0),
            }
        elif name == "get_return_policy":
            slug = "cedar" if scenario["tenant"] == "cedar" else "northstar"
            policy_path = DIRECTORY / "policies" / (slug + "-" + scenario["policy_variant"] + ".md")
            result = {
                "status": "found", "retailer": scenario["retailer"],
                "policy_version": policy_path.stem, "content": policy_path.read_text(),
            }
        elif name == "check_return_eligibility":
            result = {
                "status": scenario["eligibility"], "order_id": scenario["order_id"],
                "reason": "eligible under backend rules" if scenario["eligibility"] == "eligible"
                else "outside backend return rules or requires review",
            }
        elif name == "issue_refund":
            outcome = scenario["refund_outcome"]
            result = {"status": outcome, "order_id": scenario["order_id"]}
            if outcome == "completed":
                result["refund_id"] = "ref-" + scenario["order_id"].lower()
                result["settlement_timing"] = "5–7 business days" if scenario["tenant"] == "cedar" \
                    else "3–5 business days"
            elif outcome == "timeout":
                result["error"] = "refund service timed out; completion is unknown"
            else:
                result["reason"] = "refund backend rejected the request"
        elif name == "escalate_to_human":
            result = {"status": "queued", "order_id": scenario["order_id"],
                      "queue": "returns-review", "reason": arguments["reason"]}
        else:
            result = {"status": "error", "error": "unknown tool"}
        self.calls.append({"name": name, "arguments": arguments, "result": result})
        return result


def response_text(response):
    parts = []
    for item in response.get("output", []):
        if item.get("type") != "message":
            continue
        for content in item.get("content", []):
            if content.get("type") == "output_text":
                parts.append(content.get("text", ""))
    return "\n".join(parts).strip()


def span_row(run, scenario, trace_id, span_id, parent_id, name, started, duration_ms,
             input_value="", output_value="", tool_name="", tool_arguments="", tool_result="",
             status="OK", input_tokens=0, output_tokens=0, response_id=""):
    return {
        "Timestamp": started, "TraceId": trace_id, "SpanId": span_id,
        "ParentSpanId": parent_id, "SpanName": name,
        "TenantId": scenario["tenant"], "AgentName": "support-agent",
        "Environment": "controlled-demo", "SessionId": run["session_id"],
        "ScenarioId": scenario["id"], "DatasetVersion": run["dataset_version"],
        "RunnerVersion": run["runner_version"], "PromptVersion": run["prompt_version"],
        "PolicyVersion": scenario["policy_variant"], "Input": input_value,
        "Output": output_value, "ToolName": tool_name, "ToolArguments": tool_arguments,
        "ToolResult": tool_result, "StatusCode": status, "DurationMs": duration_ms,
        "InputTokens": input_tokens, "OutputTokens": output_tokens,
        "ResponseId": response_id,
    }


def evaluate(scenario, world, final_text):
    refund_results = [call["result"] for call in world.calls if call["name"] == "issue_refund"]
    statuses = {result.get("status") for result in refund_results}
    completion_text = re.sub(
        r"\b(?:if|once|when)\s+(?:the\s+)?refund\s+(?:has been|was|is)\s+"
        r"(?:successfully\s+)?(?:processed|issued|completed)",
        "", final_text, flags=re.IGNORECASE)
    claimed_success = bool(re.search(
        r"refund (?:has been|was|is) (?:successfully )?(?:processed|issued|completed)|issued your refund",
        completion_text, re.IGNORECASE))
    settlement_explained = "business day" in final_text.lower()
    if "rejected" in statuses and claimed_success:
        observed = "refund_promised_after_rejection"
    elif scenario["policy_variant"] == "outdated":
        observed = "outdated_return_policy"
    elif "completed" in statuses and not settlement_explained:
        observed = "refund_succeeded_customer_returns"
    elif "timeout" in statuses:
        observed = "backend_timeout"
    elif scenario["eligibility"] == "rejected" and not claimed_success:
        observed = "correct_refusal"
    elif "completed" in statuses:
        observed = "ordinary_success"
    else:
        observed = "needs_review"
    return {
        "scenario_id": scenario["id"], "expected_family": scenario["expected_family"],
        "observed_family": observed, "matches_expected": observed == scenario["expected_family"],
        "claimed_refund_success": claimed_success,
        "refund_statuses": sorted(status for status in statuses if status),
        "settlement_timing_explained": settlement_explained,
        "policy_variant": scenario["policy_variant"],
    }


def run_scenario(client, model, run, scenario):
    trace_id = uuid.uuid4().hex
    root_id = uuid.uuid4().hex[:16]
    started = utc_now()
    started_clock = time.monotonic()
    world = RetailWorld(scenario)
    spans = []
    conversation = [{"role": "user", "content":
                     "Retailer: %s\nOrder: %s\nCustomer: %s" %
                     (scenario["retailer"], scenario["order_id"], scenario["customer_message"])}]
    total_input_tokens = 0
    total_output_tokens = 0
    final_text = ""

    for turn in range(MAX_AGENT_TURNS):
        call_started = utc_now()
        call_clock = time.monotonic()
        response = client.create({
            "model": model, "store": False, "instructions": INSTRUCTIONS,
            "input": conversation, "tools": TOOLS, "parallel_tool_calls": False,
            "metadata": {"dataset_version": run["dataset_version"],
                         "scenario_id": scenario["id"]},
        })
        usage = response.get("usage") or {}
        input_tokens = usage.get("input_tokens", 0) or 0
        output_tokens = usage.get("output_tokens", 0) or 0
        total_input_tokens += input_tokens
        total_output_tokens += output_tokens
        spans.append(span_row(
            run, scenario, trace_id, uuid.uuid4().hex[:16], root_id,
            getattr(client, "span_name", "model.create"), call_started,
            int((time.monotonic() - call_clock) * 1000),
            input_value=json.dumps(conversation, separators=(",", ":")),
            output_value=json.dumps(response.get("output", []), separators=(",", ":")),
            input_tokens=input_tokens, output_tokens=output_tokens,
            response_id=response.get("id", ""),
        ))
        output = response.get("output", [])
        conversation.extend(output)
        function_calls = [item for item in output if item.get("type") == "function_call"]
        if not function_calls:
            final_text = response_text(response)
            break
        for call in function_calls:
            tool_started = utc_now()
            tool_clock = time.monotonic()
            try:
                arguments = json.loads(call.get("arguments", "{}"))
                result = world.execute(call["name"], arguments)
                status = "ERROR" if result.get("status") in {"error", "timeout"} else "OK"
            except (KeyError, ValueError, TypeError) as error:
                arguments = {}
                result = {"status": "error", "error": str(error)}
                status = "ERROR"
            result_json = json.dumps(result, separators=(",", ":"))
            spans.append(span_row(
                run, scenario, trace_id, uuid.uuid4().hex[:16], root_id,
                "tool." + call.get("name", "unknown"), tool_started,
                int((time.monotonic() - tool_clock) * 1000),
                tool_name=call.get("name", ""),
                tool_arguments=json.dumps(arguments, separators=(",", ":")),
                tool_result=result_json, status=status,
            ))
            conversation.append({"type": "function_call_output", "call_id": call["call_id"],
                                 "output": result_json})
    else:
        raise RuntimeError("scenario %s exceeded %d agent turns" % (scenario["id"], MAX_AGENT_TURNS))

    if not final_text:
        raise RuntimeError("scenario %s ended without a final response" % scenario["id"])
    spans.insert(0, span_row(
        run, scenario, trace_id, root_id, "", "support-agent", started,
        int((time.monotonic() - started_clock) * 1000),
        input_value=scenario["customer_message"], output_value=final_text,
        input_tokens=total_input_tokens, output_tokens=total_output_tokens,
    ))
    judgment = evaluate(scenario, world, final_text)
    judgment.update({"trace_id": trace_id, "session_id": run["session_id"],
                     "dataset_version": run["dataset_version"]})
    return spans, judgment


def write_jsonl(path, rows):
    with path.open("w") as output:
        for row in rows:
            output.write(json.dumps(row, ensure_ascii=False, separators=(",", ":")) + "\n")


def expand_scenarios(scenarios, variants):
    """Create deterministic order/message variations while preserving each scenario's outcome."""
    expanded = []
    for variant in range(variants):
        for position, source in enumerate(scenarios):
            scenario = dict(source)
            prefix, number = source["order_id"].rsplit("-", 1)
            scenario["order_id"] = "%s-%04d" % (prefix, int(number) + variant * 100)
            if variant:
                scenario["id"] = "%s-v%02d" % (source["id"], variant + 1)
                scenario["customer_message"] = "%s %s" % (
                    source["customer_message"], VARIANT_CONTEXT[variant % len(VARIANT_CONTEXT)])
            items = TENANT_ITEMS[source["tenant"]]
            scenario["item"] = items[(position + variant) % len(items)]
            scenario["order_total_usd"] = round(39 + ((position * 23 + variant * 17) % 260) + .95, 2)
            expanded.append(scenario)
    return expanded


def run_with_retries(client, model, run, scenario, retries):
    for attempt in range(retries + 1):
        try:
            return run_scenario(client, model, run, scenario)
        except RuntimeError:
            if attempt == retries:
                raise
            print("retrying %s after attempt %d" % (scenario["id"], attempt + 1),
                  file=sys.stderr)


def main(argv=None, client=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenarios", type=Path, default=DEFAULT_SCENARIOS)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--provider", choices=("openai", "anthropic", "claude-cli"),
                        default="openai")
    parser.add_argument("--model")
    parser.add_argument("--only", action="append", default=[], help="run only this scenario id")
    parser.add_argument("--variants", type=int, default=1,
                        help="deterministic order/message variants per selected scenario")
    parser.add_argument("--dataset-version", help="override the scenario file's dataset version")
    parser.add_argument("--jobs", type=int, default=1,
                        help="number of independent scenarios to execute concurrently")
    parser.add_argument("--retries", type=int, default=2,
                        help="retries for a failed model execution")
    args = parser.parse_args(argv)
    if args.variants < 1 or args.variants > 50:
        parser.error("--variants must be between 1 and 50")
    if args.jobs < 1 or args.jobs > 8:
        parser.error("--jobs must be between 1 and 8")
    if args.retries < 0 or args.retries > 5:
        parser.error("--retries must be between 0 and 5")
    spec = json.loads(args.scenarios.read_text())
    scenarios = [item for item in spec["scenarios"] if not args.only or item["id"] in args.only]
    if not scenarios:
        parser.error("no scenarios selected")
    scenarios = expand_scenarios(scenarios, args.variants)
    protected = [args.output_dir / name for name in
                 ("spans.jsonl", "evaluations.jsonl", "manifest.json")]
    if any(path.exists() for path in protected):
        parser.error("output artifacts already exist; choose a new output directory")
    defaults = {"openai": DEFAULT_MODEL, "anthropic": ANTHROPIC_MODEL,
                "claude-cli": CLAUDE_CLI_MODEL}
    model = args.model or defaults[args.provider]
    if client is None:
        if args.provider == "claude-cli":
            client = ClaudeCLIClient()
        else:
            env_name = "ANTHROPIC_API_KEY" if args.provider == "anthropic" else "OPENAI_API_KEY"
            api_key = os.environ.get(env_name)
            if not api_key:
                parser.error("%s is required for real pilot runs" % env_name)
            client = AnthropicClient(api_key) if args.provider == "anthropic" else ResponsesClient(api_key)

    run = {
        "session_id": "pilot-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ"),
        "dataset_version": args.dataset_version or spec["dataset_version"],
        "prompt_version": spec["prompt_version"],
        "runner_version": spec["runner_version"],
        "provider": getattr(client, "provider", args.provider),
    }
    started = utc_now()
    results = [None] * len(scenarios)
    with ThreadPoolExecutor(max_workers=args.jobs) as pool:
        pending = {pool.submit(run_with_retries, client, model, run, scenario, args.retries): index
                   for index, scenario in enumerate(scenarios)}
        for future in as_completed(pending):
            index = pending[future]
            results[index] = future.result()
            judgment = results[index][1]
            print("completed %s: %s" % (scenarios[index]["id"],
                                        judgment["observed_family"]), flush=True)
    spans = [span for scenario_spans, _ in results for span in scenario_spans]
    evaluations = [judgment for _, judgment in results]

    args.output_dir.mkdir(parents=True, exist_ok=True)
    write_jsonl(args.output_dir / "spans.jsonl", spans)
    write_jsonl(args.output_dir / "evaluations.jsonl", evaluations)
    manifest = {
        **run, "model": model, "started_at": started, "completed_at": utc_now(),
        "base_scenario_count": len(scenarios) // args.variants,
        "variants_per_scenario": args.variants,
        "scenario_count": len(scenarios), "trace_count": len(evaluations),
        "minimum_timestamp": min(span["Timestamp"] for span in spans),
        "maximum_timestamp": max(span["Timestamp"] for span in spans),
        "scenarios_sha256": sha256(args.scenarios),
        "runner_sha256": sha256(Path(__file__)),
        "source_manifest": "SOURCE_MANIFEST.md",
    }
    (args.output_dir / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print("wrote %d traces to %s" % (len(evaluations), args.output_dir))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, RuntimeError) as error:
        print("pilot failed: %s" % error, file=sys.stderr)
        sys.exit(1)
