# Railway retail demo maintenance

This directory contains the tools used to generate, load, configure, and verify the synthetic
retail-support dataset served by the canonical
[LightShip demo on Railway](https://lightship-production.up.railway.app/). It does not start a
second demo environment.

The demo uses two fictional retailers, one tool-calling support agent, and 12 controlled scenario
families. Investigator-visible spans and evaluator judgments are separate artifacts and separate
ClickHouse tables. LightShip connects to `lightship_demo.agent_traces` with a read-only database
identity. Source review and licensing details are recorded in
[SOURCE_MANIFEST.md](SOURCE_MANIFEST.md).

## Generate a dataset

Choose a supported model backend and record all 12 scenarios. Store generated artifacts under the
ignored `local/` directory; never commit API keys, credentials, or generated trace contents.

With an OpenAI API key:

```sh
OPENAI_API_KEY=... python3 examples/retail-demo/runner.py \
  --output-dir local/retail-demo/pilot-v1
```

The runner uses the pinned `gpt-5.4-mini-2026-03-17` snapshot by default. Each scenario produces
one trace with child spans for model requests and executed tool calls. It writes:

- `spans.jsonl`: evidence visible to an investigator.
- `evaluations.jsonl`: expected and measured outcomes; never load this into the trace table.
- `manifest.json`: versions, hashes, model, counts, and fixed dataset timestamps.

The same normalized trace contract can be recorded with Anthropic's Messages API:

```sh
ANTHROPIC_API_KEY=... python3 examples/retail-demo/runner.py \
  --provider anthropic --output-dir local/retail-demo/pilot-v1
```

If Claude Code is already signed in, its non-interactive mode can record the pilot without an API
key:

```sh
python3 examples/retail-demo/runner.py --provider claude-cli \
  --output-dir local/retail-demo/pilot-v1
```

After reviewing the pilot, record a 120-trace showcase with ten variations of every scenario:

```sh
python3 examples/retail-demo/runner.py --provider claude-cli --variants 10 --jobs 3 \
  --dataset-version retail-demo-v1 \
  --output-dir local/retail-demo/showcase-v1
```

Runs are real model executions. Scenario families are opportunities, not forced percentages. Read
the artifacts before importing them and keep the public story aligned with the observed evidence.
The recorded `retail-demo-v1` showcase has no case where the agent falsely reports a rejected
refund as completed.

## Provision the hosted trace database

Provision the dedicated ClickHouse database and scoped loader and reader identities once, using an
administrative HTTPS DSN for the hosted ClickHouse service:

```sh
CLICKHOUSE_DSN=... python3 examples/retail-demo/provision.py
```

The command refuses an existing `lightship_demo` database unless `--resume-existing` is explicit.
It writes generated credentials with mode `0600` to `local/retail-demo/clickhouse.env` and does not
print them. Configure the Railway service with the generated `LIGHTSHIP_CLICKHOUSE_DSN`. Rotate any
administrative DSN exposed through a terminal, chat, or other retained history.

## Import a reviewed dataset

Load the credentials created during provisioning, then import the reviewed artifacts:

```sh
set -a
source local/retail-demo/clickhouse.env
set +a
python3 examples/retail-demo/load.py local/retail-demo/showcase-v1
```

The loader validates the complete artifact before connecting, stages both tables, and replaces
only the named dataset partition. An identical import is a no-op. Different content with the same
version is rejected unless the operator explicitly supplies `--replace-version` for that exact
version. It never truncates or replaces a database or whole table.

## Configure and verify Railway

Run the configuration verifier against the canonical Railway deployment:

```sh
LIGHTSHIP_DEMO_ADMIN_PASSWORD=... \
python3 examples/retail-demo/configure.py local/retail-demo/showcase-v1
```

The script defaults to `https://lightship-production.up.railway.app`. Use `--base-url` only when the
canonical Railway domain changes. It binds the trace table, creates the preview roles, and verifies
pagination, fixed time bounds, Cedar/Northstar isolation, indistinguishable denied/not-found trace
details, and the recorded evidence chain.

The preview roles are:

```text
cedar-account-manager:     TenantId == "cedar" && AgentName == "support-agent"
northstar-account-manager: TenantId == "northstar" && AgentName == "support-agent"
support-operations:        AgentName == "support-agent"
```

The tenant is literal because role preview substitutes the role while retaining the admin actor's
attributes, and the protected bootstrap admin cannot be assigned attributes. Policies combine as a
union, so adding another policy can broaden access.

`LIGHTSHIP_DEMO_DATASET_FROM` and `LIGHTSHIP_DEMO_DATASET_TO` should match the manifest's immutable
timestamp bounds in Railway. They control the default **Demo dataset** range; recorded span
timestamps are never shifted or rewritten. Set `LIGHTSHIP_DEMO_MODE=true` in Railway; LightShip
rejects every other `LIGHTSHIP_DEMO_*` runtime setting unless that explicit switch is enabled.

## Run maintenance tests

```sh
python3 -m unittest discover -s examples/retail-demo -p 'test_*.py'
```

The tests use fake transports and do not contact model providers, Railway, or ClickHouse.

## Hosting boundary

The Railway demo intentionally displays its shared administrator credential and allows visitors to
edit roles, fields, source binding, and users. The account cannot change the displayed password. It
can create API keys for MCP and REST access, but the server caps their lifetime at 24 hours. Use
only synthetic traces, a dedicated disposable Postgres database, and the provisioned SELECT-only
ClickHouse reader. Do not reuse demo mode for a real environment.

Pushing the branch connected to Railway rebuilds the image and restarts the service. Railway keeps
the hosted Postgres volume and LightShip applies startup migrations. Railway's **Redeploy** action
restarts the last pushed revision; it does not load or restore the dataset.

Visitors can deliberately edit the access model. Restore the canonical binding, fields, and
preview roles without touching trace data by rerunning the configuration verifier with the Railway
admin password.
