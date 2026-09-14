-- 0002_api_keys.sql — API keys, and users that no longer come from the config file.
--
-- Users move out of the YAML and onto the API (README, "Users and keys"). Two consequences are
-- structural rather than cosmetic:
--
--   * A user may have no password. An identity provisioned for SSO has nothing to hash yet, and a
--     null hash is the honest representation of that — an empty string would be a password that
--     no argon2id verify can ever match, which is the same outcome reached by accident.
--   * Provisioning is now something a script does unattended, so there has to be a credential that
--     is not a browser session.

alter table app_user alter column password_hash drop not null;

-- A key is a credential, never a second authorization mechanism. It resolves to the same roles and
-- attributes the query path already reads, so the guarantee is unchanged: a key cannot return a row
-- its principal's roles do not permit.
--
-- Every key belongs to a user. Automation that must outlive any particular operator is a
-- service-account user — an ordinary row nobody signs into interactively — which gets attributes,
-- role scoping, revocation and audit attribution for free, where a user-less key would need each
-- of those reinvented beside the identity model that carries the guarantee.
create table api_key (
  id           uuid primary key default gen_random_uuid(),
  name         text        not null,
  token_sha    text        not null unique,   -- sha256 of the token; the token itself is never stored
  user_id      uuid        not null references app_user(id) on delete cascade,
  created_by   text        not null,          -- denormalised: the audit trail outlives the creator
  created_at   timestamptz not null default now(),
  expires_at   timestamptz,                   -- null: no expiry
  last_used_at timestamptz,
  revoked_at   timestamptz
);
create index on api_key (user_id);
