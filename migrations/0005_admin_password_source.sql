-- Remember the last environment-managed admin hash separately from the current credential.
-- A self-service password change can then survive restarts, while changing the environment secret
-- still acts as the documented break-glass rotation.
alter table app_user add column admin_env_password_hash text;
