# Integration tests

The suite exercises migrations, API-driven setup, enrollment, login, policy translation, whole-trace
queries, pagination, MCP, and auditing against real Postgres and ClickHouse. It starts an in-process
HTTP server; there is no config file or separate `lightship serve` process to configure.

## Run against disposable databases

Use dedicated test instances. The suite mutates the Postgres access model and users, and drops and
recreates its fixed ClickHouse fixture table. Do not point either connection at production.

For example, start disposable local instances:

```sh
docker run --rm -d --name lightship-test-postgres -p 127.0.0.1:55433:5432 \
  -e POSTGRES_USER=lightship -e POSTGRES_PASSWORD=lightship -e POSTGRES_DB=lightship postgres:16
docker run --rm -d --name lightship-test-clickhouse -p 127.0.0.1:59000:9000 \
  -e CLICKHOUSE_SKIP_USER_SETUP=1 clickhouse/clickhouse-server:24.8-alpine
```

Wait until both checks succeed:

```sh
docker exec lightship-test-postgres pg_isready -U lightship -d lightship
docker exec lightship-test-clickhouse clickhouse-client --query 'SELECT 1'
```

Then run from the repository root:

```bash
mkdir -p local
cp integration/env.example local/test.env
source local/test.env
go test ./integration -count=1 -v
```

`TestFullStack` skips unless both variables are set. It applies migrations, creates the test admin,
and configures the binding, marked fields, roles, and test users through the API. The fixture table
is always `otel.lightship_integration_test`; it is not selected by an environment variable.

To include the ClickHouse-backed policy and pagination tests as well:

```sh
go test ./... -count=1
```

The ClickHouse test tables are `otel.lightship_integration_test`, `otel.lightship_property_test`, and
`otel.lightship_pagination_test`. `internal/testtable` rejects destructive test-table operations
unless the name ends in `_test`. This guards table naming, not the database connection: a test
pointed at a production cluster still creates tables and consumes resources.

When finished, stopping the disposable containers removes their test state:

```sh
docker stop lightship-test-postgres lightship-test-clickhouse
```

## Authorization and query assertions

| Scenario | Expected behavior |
|---|---|
| Two users, one role, different attributes | A policy bound to `user.tenant_id` gives caller-relative access. |
| At least one span matches a policy | The complete trace is visible. `untagged-1` is visible to Alok despite its untagged span; `mixed-1` is visible to both tenants. |
| Denied versus nonexistent trace | Both return 404; trace detail does not reveal which case occurred. |
| Client filters | Conditions narrow the authorized trace set; one span must satisfy the ANDed conditions, and the selected trace is returned whole. |
| Bulk row reads | The row query independently applies authorization and preserves projection and whole-trace semantics. |
| Keyset pagination | Scrolling returns each visible trace once, including traces whose timestamps share a second. |
| Analysis MCP | `POST /mcp` exposes the same analysis tools to users and admins; it has no setup tools. |
| Setup MCP and administration | Only admins can inspect the audit log, invoke administrative REST operations, or use `POST /mcp/setup`. |

The local stdio companion and its workspace export behavior are covered by unit tests. This suite
exercises the remote analysis and Setup MCP endpoints against the real databases.

For manual setup against a real corpus, follow the root [README](../README.md). The integration
fixture credentials and destructive test workflow are not a deployment configuration.
