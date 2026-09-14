# Retail demo source manifest

Reviewed and pinned 2026-09-08.

## Sierra τ²-bench

- Repository: <https://github.com/sierra-research/tau2-bench>
- Revision: [`672227c6b6676edc20d57ea53b7000262aae77b9`](https://github.com/sierra-research/tau2-bench/commit/672227c6b6676edc20d57ea53b7000262aae77b9)
- License: MIT, Copyright © 2025 Sierra Research. See the [license at the pinned revision](https://github.com/sierra-research/tau2-bench/blob/672227c6b6676edc20d57ea53b7000262aae77b9/LICENSE).
- Reviewed material: retail tasks 10, 46, and 53; `policy.md`; and the retail environment and
  tool interfaces.

This demo is a clean-room adaptation of those task concepts. It uses new fictional retailers,
orders, policies, prompts, tool implementations, and expected outcomes. No upstream person-shaped
records, source code, or policy prose are copied. The combined upstream return operation was split
into lookup, policy retrieval, eligibility, refund, and escalation tools; controlled policy
versions and backend outcomes were added for the investigation.

## Langfuse agent workshop

- Repository: <https://github.com/langfuse/langfuse-workshop>
- Revision: [`dfda74836f2f18527633b1714121823be815e7e9`](https://github.com/langfuse/langfuse-workshop/commit/dfda74836f2f18527633b1714121823be815e7e9)
- License status at review: no `LICENSE` or `COPYING` file, no package license, and GitHub reported
  no detected license.

The workshop was used only to confirm the architectural pattern of one observed agent loop with
observed tools. No code, fixtures, documentation, dependencies, or assets were copied. The runner
implements its own Responses API loop with the Python standard library and writes directly to the
demo's documented interchange files. It does not write to or infer the schema of Langfuse's
operational tables.

## OpenAI API

The runner uses the Responses API with custom function tools and defaults to the pinned
`gpt-5.4-mini-2026-03-17` model snapshot. It sets `store: false` and records response identifiers,
model-reported token usage, executed tool calls, and final output. It does not estimate cost.
