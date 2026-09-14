# Contributing to LightShip

LightShip is experimental software. Contributions that make authorization behavior easier to
verify, fix correctness issues, or improve setup and documentation are especially useful.
Discuss new features in an issue before implementing them. Keep each pull request focused.

## Report a problem

For ordinary bugs, use this repository's bug-report template with a minimal reproduction and
expected versus actual behavior. Use synthetic traces, fake identities, and sanitized logs.
Never include API keys, database URLs with credentials, production traces, or customer data.
For suspected vulnerabilities, follow [SECURITY.md](SECURITY.md), not a public issue.

## Development setup

- Go: a patched Go 1.25 toolchain (CI uses the latest `1.25.x`); see [go.mod](go.mod).
- Node.js 22 or newer for the JavaScript tests; no npm dependencies are required.
- Docker with Compose for integration tests and container builds.
- Python 3 for the hosted-demo dataset tests.

From a clone of this repository:

```sh
go mod download
go build -o lightship-bin ./cmd/lightship
```

The server uses Postgres for configuration and identity and ClickHouse for trace data. Never use
production databases for tests: the integration suite changes users and access policies and
recreates fixtures. Use the hosted Railway demo when you want to try the product without supplying
your own trace source.

## Run checks

For the checks that need no database, first unset test/database connection variables in your shell:

```sh
unset DATABASE_URL LIGHTSHIP_TEST_CLICKHOUSE
go vet ./...
go test -race ./... -count=1
node --experimental-vm-modules --test internal/httpapi/ui_admin_test.mjs internal/httpapi/ui_traces_test.mjs
python3 -m unittest discover -s examples/retail-demo -p 'test_*.py'
```

Format modified Go files with `gofmt`. Follow [integration/README.md](integration/README.md) to
start disposable databases, then run `go test -race -p 1 ./... -count=1`. Database tests skip if
their connection variables are missing; a skipped test is not proof that SQL behavior works.
CI runs both the isolated checks and the real-database suite.

## Code map and review expectations

| Area | Responsibility |
|---|---|
| `internal/expression` | Shared typed AST, capability validation, parameterized predicate compiler |
| `internal/policy`, `internal/filter` | CEL and structured JSON adapters to the shared expressions |
| `internal/traces` | ClickHouse discovery and authorized trace queries |
| `internal/auth`, `internal/store`, `internal/model` | Credentials, persisted configuration, compiled model cache |
| `internal/httpapi` | REST, remote analysis and Setup MCP endpoints, and embedded UI |
| `internal/localmcp` | Local stdio MCP proxy and atomic workspace exports |
| `migrations`, `integration` | Postgres schema and real-database regression tests |

For authorization-related changes, explain and test the affected guarantees:

- Filters may narrow access, never grant it. Policy and filter capability profiles remain separate.
- Every row-returning path authorizes independently; a cursor or previously selected trace ID grants
  no authority. One matching span grants the **whole trace**, not just the matching span.
- Missing, null, malformed, and negated values must have explicit, tested behavior.
- User values are SQL parameters; identifiers must be validated before interpolation.
- Configuration changes, cache behavior, and credential revocation need concurrency/error-path tests.
- Preserve the existing JSON wire format and cursor canonicalization unless an intentional breaking
  change is discussed and documented.

Add a regression test for a bug fix. For SQL changes, test returned results against ClickHouse in
addition to SQL strings. For UI changes, run the Node tests and describe any manual browser checks.
Document limitations and migrations alongside behavior changes. Comments explain behavior for
future readers; PR descriptions explain the reason for the change.

## Submitting a pull request

1. Branch from the current default branch of the destination repository.
2. Make one focused change with tests and documentation as needed.
3. Use the PR template; report the exact checks run and any skips.
4. Wait for CI and maintainer review before merging. Do not put the change on the default branch
   before requesting review.

AI-assisted contributions are welcome, but contributors must understand and verify the code they
submit. Do not paste private data into external tools. Generated code is subject to the same review
and licensing requirements as handwritten code.

Only submit material you have the right to contribute. Contributions intentionally submitted for
inclusion are under [Apache-2.0](LICENSE), as described in section 5 of that license. Existing
third-party notices must be preserved. Follow the [Code of Conduct](CODE_OF_CONDUCT.md).
