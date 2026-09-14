# Changelog

## v0.1.0-alpha.1 — 2026-09-10

This initial prerelease is for evaluation and is not a production-readiness guarantee.

### Included

- A self-hosted access-control proxy for OpenTelemetry traces stored in ClickHouse.
- Role-based, caller-relative policies; whole-trace authorization; REST, MCP, and a web UI.
- Separate analysis and administrator-only Setup MCP endpoints.
- A local stdio MCP companion that streams authorized traces to workspace JSONL files while
  returning only export metadata to the conversation.
- A shared typed expression compiler with separate policy and filter capability profiles.
- String, string-array, and boolean policies; broader typed filters including numbers.
- Postgres-backed configuration, sessions, personal API keys, and audit records.
- Contributor guidance, automated checks, and a synthetic-data evaluation demo.

### Limitations

- APIs, database schemas, and policy semantics may change during alpha.
- No claim of production security, exhaustive compatibility, or independent security certification.
- Discovery identifier validation, unbounded trace buffering, physical-type enforcement, and cursor
  time-window stability need further work.
- Local exports contain complete authorized trace payloads and must remain private, ignored by
  version control, and removed when no longer needed.
- The hosted demo contains invented data and disposable public credentials.

See [SECURITY.md](SECURITY.md) before evaluating and [RELEASING.md](RELEASING.md) before publishing.

### Security fixes

- Model writes reject stale configuration versions with HTTP 409; reads use a consistent snapshot.
- Expired policy caches fail closed on reload errors. Failed immediate post-write refreshes discard
  the local model. The TTL includes database read and compilation time.
- Password-hash imports and verification require the fixed Argon2id profile produced by the CLI;
  malformed and unsupported hashes are rejected without running Argon2. Previously imported hashes
  using other profiles must be replaced before upgrading.
