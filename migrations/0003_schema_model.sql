-- 0003_schema_model.sql — the access model moves out of the config file.
--
-- Roles, policies, the source binding and the field whitelist are all rows now. There is one source
-- of truth rather than two trying to converge, which is what deletes the reconciliation problem
-- rather than solving it: nothing has to decide what a boot does with a row the file no longer
-- mentions, because there is no file.
--
-- It also improves the failure mode. A policy that will not compile used to stop the process from
-- booting — a deployment outage caused by a typo. It is now rejected at the write that proposes it,
-- and the running system is untouched.

-- Which table holds spans, and which of its columns play which structural role. A single row: one
-- deployment fronts one table.
--
-- Every role is required because none is discoverable away. The guarantee is
-- `GROUP BY <trace_id> HAVING countIf(policy) > 0`; without knowing which column groups spans
-- into traces there is no quantifier to enforce. Required-ness is enforced in Validate on the
-- write path, where every other model rule lives, not as a check constraint here.
create table source_binding (
  only_row       boolean primary key default true check (only_row),
  table_name     text        not null,
  trace_id       text        not null,
  ts             text        not null,
  span_id        text        not null default '',
  parent_span_id text        not null default '',
  span_name      text        not null default '',
  updated_at     timestamptz not null default now()
);

-- The closed set of things a policy or a filter may name. map_column is '' for a scalar column, and
-- otherwise names the Map column the key lives in.
--
-- The two flags are independent. filterable lets a caller narrow their own query and can only
-- shrink what they already see. policy_ref admits the field to the vocabulary of the security
-- model — a policy naming a field without it is rejected before it is stored.
create table field (
  id          uuid    primary key default gen_random_uuid(),
  map_column  text    not null default '',
  name        text    not null,
  filterable  boolean not null default false,
  policy_ref  boolean not null default false,
  unique (map_column, name)
);

-- Caller attributes a policy may compare against, as user.<name>. Was `user_attributes` in the
-- config file; the same closed-set rule, now a table.
create table user_attribute_key (
  name text primary key
);

-- Every mutation to the access model bumps this. Replicas hold a compiled snapshot in memory with a
-- short TTL rather than reading Postgres per request, so the version is how a reload announces that
-- it picked up a change — and how an operator can tell whether two replicas agree.
create table model_version (
  only_row boolean primary key default true check (only_row),
  version  bigint  not null default 1
);
insert into model_version (only_row) values (true);

-- The bootstrap admin credential can now be generated at first boot and printed to the log once
-- (internal/store/bootstrap.go). That print is the one place the password exists in the clear, in
-- a stream a log collector may ship elsewhere — so the first login on a generated credential must
-- be distinguishable in the audit log from every later one. This flag is set at generation and
-- consumed by the first successful login, which is recorded as auth.bootstrap_first_login rather
-- than auth.login. It lives on the user row rather than being derived from audit history, because
-- audit rows are pruned by retention and the marker must not quietly expire with them. Rotating
-- the credential via LIGHTSHIP_ADMIN_PASSWORD_HASH clears it: the printed password is no longer
-- the live one, so a login after rotation is ordinary.
alter table app_user add column bootstrap_login_pending boolean not null default false;

-- A user provisioned through POST /users now gets a randomly generated password that the admin
-- reads once and hands over, rather than a hash the admin computed and therefore knows forever.
-- This flag is what makes that handover a handover: while it is set the user may do nothing but
-- read GET /me, change their password, and sign out, so the credential the admin saw stops working
-- as soon as it has been used once. It defaults to false because the column is added to a table
-- that already holds the bootstrap admin — an account whose credential nobody else ever saw, and
-- which must not be swept into an enrollment it has no reason to perform. POST /users defaults it
-- to true at the write instead, where the distinction between an enrolled person and a service
-- account nobody signs into is actually known.
alter table app_user add column must_change_password boolean not null default false;
