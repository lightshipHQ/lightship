# LightShip

**Faster production-agent iteration, without a developer bottleneck.**

[![CI](https://github.com/lightshipHQ/lightship/actions/workflows/ci.yml/badge.svg)](https://github.com/lightshipHQ/lightship/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/lightshipHQ/lightship?include_prereleases&sort=semver)](https://github.com/lightshipHQ/lightship/releases)
[![Container](https://img.shields.io/badge/container-ghcr.io%2FlightshipHQ%2Flightship-blue)](https://github.com/lightshipHQ/lightship/pkgs/container/lightship)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

LightShip gives domain experts governed, self-serve access to production agent traces. It sits in
front of your existing OpenTelemetry data in ClickHouse, so humans and AI coding agents can
investigate real conversations and tool calls without database credentials or another observability
silo.

[Documentation](https://lightship.mintlify.site) ·
[Website](https://lightship.sh/) ·
[Try the hosted demo](https://lightship-production.up.railway.app/) ·
[Contribute](CONTRIBUTING.md)

## Why LightShip

- **Find the patterns behind failures.** Ask business questions across conversations, tool calls,
  and outcomes using Codex, Claude, or another MCP client.
- **Keep your existing trace store.** Bind LightShip to your OpenTelemetry table without copying
  traces into a separate platform.
- **Instrument once; define policies later.** Select existing columns and attribute keys as
  authorization inputs, then create or change policies without re-instrumenting. The same policy
  governs the web UI, REST API, and MCP.
- **Keep large investigations out of model context.** The local MCP companion writes authorized
  exports into the agent workspace and returns only paths and counts to the conversation.

## Try the hosted demo

Open the [hosted demo](https://lightship-production.up.railway.app/). Its sign-in page provides the
credentials for a disposable administrator account. The dataset contains 132 recorded synthetic
support-agent conversations across two fictional retailers.

In the web UI you can:

- inspect traces, conversations, and tool results;
- preview the tenant-scoped access available to each account team;
- review searchable and policy-accessible fields; and
- create a short-lived API key to investigate the same data through MCP.

Follow the [tenant-scoped access guide](https://lightship.mintlify.site/guides/tenant-scoped-trace-access)
to explore the access-control setup and verify its boundary. To investigate the traces
conversationally, follow the [MCP connection guide](https://lightship.mintlify.site/use/mcp), then
use the tested prompt sequence in the tenant-scoped access guide.

The deployment is an intentionally editable sandbox. Do not enter real or sensitive data.

## How it works

1. An administrator binds LightShip to an existing OpenTelemetry span table in ClickHouse.
2. They choose which columns and map keys may be used for search and access rules.
3. LightShip compiles each caller’s roles and attributes into the ClickHouse query before returning
   any trace through the web UI, REST, or MCP.

A trace is the authorization unit: **one matching span grants access to the complete trace**.
LightShip never gives restricted users direct ClickHouse credentials.

## Performance

Query performance depends on the layout and indexes of your existing ClickHouse trace table. After
you select the fields available to filters and access policies, LightShip reviews the table's
sorting key and skip indexes and suggests DDL for missing indexes. These recommendations are
advisory: LightShip never changes your ClickHouse schema automatically. See how to
[review the optimization report](https://lightship.mintlify.site/guides/tenant-scoped-trace-access#5-review-the-clickhouse-optimization-report).

## Get started

The documentation contains the maintained setup and reference material:

- [Install the latest alpha](https://github.com/lightshipHQ/lightship/releases)
- [Browse the Go package](https://pkg.go.dev/github.com/lightshipHQ/lightship)
- [Connect your ClickHouse](https://lightship.mintlify.site/quickstart)
- [Define roles and policies](https://lightship.mintlify.site/configure/roles-and-policies)
- [Connect Codex, Claude, and other MCP clients](https://lightship.mintlify.site/use/mcp)
- [Use the web UI](https://lightship.mintlify.site/use/web-ui)
- [Deploy and configure LightShip](https://lightship.mintlify.site/operate/deployment)
- [Browse the API reference](https://lightship.mintlify.site/api-reference/introduction)

Need help deploying LightShip in your environment? [Talk to us](https://cal.com/lightship/30min).

## Security and project status

LightShip protects only requests sent through LightShip. Direct ClickHouse access bypasses its
policies. It does not currently detect PII, redact payloads, or hide individual spans. Query
auditing is best-effort, so an audit-write failure is logged without failing the trace request.

LightShip is an experimental alpha with no stable release. Evaluate it using synthetic or
non-sensitive data in an isolated environment. Read the [security guidance](SECURITY.md) and
[current limitations](https://lightship.mintlify.site/operate/limitations) before deployment.

## Feedback

We want to help make autonomous work trustworthy and governable. If your team operates AI agents
in production, we would love to hear what LightShip should support next.

[Talk to us](https://cal.com/lightship/30min) or
[open a GitHub issue](https://github.com/lightshipHQ/lightship/issues/new/choose).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development and testing,
[SECURITY.md](SECURITY.md) for vulnerability reporting, and [CHANGELOG.md](CHANGELOG.md) for changes.
LightShip is licensed under [Apache-2.0](LICENSE).
