# Security policy

## Experimental status

LightShip is an experimental authorization proxy, not a production-certified security boundary.
There is no stable release or security-response SLA. Fixes currently target the default branch;
there are no maintained historical release branches. Evaluate with synthetic or non-sensitive data
in an isolated environment. Only the explicitly disposable demo mode is intended for public access.

## Report a vulnerability privately

Do not disclose suspected authorization bypasses, exploitable crashes, credentials, or private
trace data in public issues, discussions, or pull requests.

Use **Security → Advisories → Report a vulnerability** in this repository.
Include:

- The commit or version and relevant deployment configuration, with secrets removed.
- A minimal reproduction using synthetic data.
- Expected and actual behavior, impact, and any mitigation you have verified.

If GitHub private vulnerability reporting is unavailable, email
[akanksha@thewoven.ai](mailto:akanksha@thewoven.ai). Do not include production credentials or
customer trace data in the initial report. No security-response SLA is currently claimed.

## Boundaries and known limitations

- LightShip only protects queries sent through it. Direct ClickHouse access bypasses its policies.
- Authorization is trace-wide: any matching span permits the complete trace, including unmatched
  spans and payloads. LightShip does not redact or classify data.
- Local MCP exports under `.lightship/exports/` contain complete authorized trace payloads. Keep the
  directory ignored, restrict workspace access, delete exports when they are no longer needed, and
  never commit or upload them.
- The local MCP process receives `LIGHTSHIP_API_KEY` through its environment. Supply it through a
  trusted secret-injection path; do not put it in tool arguments, logs, or shared configuration.
- The export tool returns only file metadata, but a later agent command can still read the exported
  rows into its context. Use bounded queries and ask the agent to analyze the file locally rather
  than printing it.
- Use read-only ClickHouse credentials. For any non-local evaluation, configure TLS, replace demo
  credentials, restrict network access, and protect Postgres and backups.
- Access-model updates use version-checked transactions. Concurrent stale edits return 409 without
  changing stored state; readers load one consistent snapshot.
- `LIGHTSHIP_MODEL_TTL` is the maximum cache age for new authorization decisions. Expired caches
  fail closed if refresh fails, and failed post-write refreshes discard the local model immediately.
  This does not cancel queries already authorized and in flight. Database outages can deny access.
- Imported password hashes must match `lightship hash`'s fixed Argon2id profile (v19, 64 MiB,
  3 iterations, 4 lanes, 16-byte salt, 32-byte digest). Malformed or other profiles are rejected
  before storage and again before verification. Password verification has a per-process concurrency
  cap; per-client and distributed login rate limiting remains an edge requirement.
- Hosted demo mode deliberately displays one ordinary account credential. If that account is an
  administrator, visitors can change roles, fields, source binding, and users. The displayed account
  cannot change its own password. It can create API keys that expire within 24 hours; those bearer
  tokens authenticate both MCP and REST requests with the administrator's current privileges. Its
  control-plane state remains disposable and must be reset after untrusted use. Never connect this
  mode to sensitive data or shared infrastructure.
- Discovery accepts only plain `database.table` identifiers, but schema configuration remains a
  highly privileged administrative operation.
- Whole-trace responses are buffered without a row/byte bound. A large trace can exhaust memory.
- Physical/logical field-type compatibility and schema drift are not yet fully enforced.
- Login and trace requests have per-process concurrency caps, and trace requests have a deadline
  and maximum time window. These are not distributed or per-client rate limits. Do not expose the
  login endpoint without appropriate external controls.

These limitations are not fixed by packaging the code as an alpha release. See
[RELEASING.md](RELEASING.md) for publication checks.
