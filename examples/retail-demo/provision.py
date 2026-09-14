#!/usr/bin/env python3
"""Provision the exact hosted-demo ClickHouse database and scoped identities."""

import argparse
import os
from pathlib import Path
import secrets
import stat
import sys
import urllib.parse

from load import ClickHouse


DIRECTORY = Path(__file__).resolve().parent
DEFAULT_SECRETS = DIRECTORY.parent.parent / "local" / "retail-demo" / "clickhouse.env"
DATABASE = "lightship_demo"
LOADER = "lightship_demo_loader"
READER = "lightship_demo_reader"
READER_SETTINGS = (
    "readonly = 2, "
    "max_concurrent_queries_for_user = 4 MAX 4, "
    "max_execution_time = 35 MAX 35, "
    "max_memory_usage = 536870912 MAX 536870912, "
    "max_result_rows = 100000 MAX 100000, "
    "max_result_bytes = 67108864 MAX 67108864, "
    "result_overflow_mode = 'throw' READONLY"
)


def sql_string(value):
    return "'" + value.replace("\\", "\\\\").replace("'", "\\'") + "'"


def schema_statements(path):
    text = path.read_text()
    return [statement.strip() for statement in text.split(";") if statement.strip()]


def credentials(path):
    if path.exists():
        values = {}
        for line in path.read_text().splitlines():
            if line and not line.startswith("#") and "=" in line:
                key, value = line.split("=", 1)
                values[key] = value
        loader_password = values.get("LOADER_PASSWORD") or values.get(
            "LIGHTSHIP_DEMO_CLICKHOUSE_PASSWORD")
        reader_password = values.get("READER_PASSWORD")
        if not reader_password and values.get("LIGHTSHIP_CLICKHOUSE_DSN"):
            reader_password = urllib.parse.unquote(
                urllib.parse.urlparse(values["LIGHTSHIP_CLICKHOUSE_DSN"]).password or "")
        if loader_password and reader_password:
            return loader_password, reader_password
        raise RuntimeError("existing secrets file is incomplete: %s" % path)
    return secrets.token_urlsafe(32), secrets.token_urlsafe(32)


def write_credentials(path, admin_dsn, loader_password, reader_password):
    parsed = urllib.parse.urlparse(admin_dsn)
    port = parsed.port or (443 if parsed.scheme == "https" else 80)
    host = "%s://%s:%d" % (parsed.scheme, parsed.hostname, port)
    loader_dsn = "%s://%s:%s@%s:%d/%s?secure=true" % (
        parsed.scheme, LOADER, urllib.parse.quote(loader_password, safe=""), parsed.hostname,
        port, DATABASE)
    reader_dsn = "%s://%s:%s@%s:%d/%s?secure=true" % (
        parsed.scheme, READER, urllib.parse.quote(reader_password, safe=""), parsed.hostname,
        port, DATABASE)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        "# Generated retail-demo ClickHouse credentials. Do not commit or print.\n"
        "LIGHTSHIP_DEMO_CLICKHOUSE_URL=%s\n" % host +
        "LIGHTSHIP_DEMO_CLICKHOUSE_DATABASE=%s\n" % DATABASE +
        "LIGHTSHIP_DEMO_CLICKHOUSE_USER=%s\n" % LOADER +
        "LIGHTSHIP_DEMO_CLICKHOUSE_PASSWORD=%s\n" % loader_password +
        "LOADER_PASSWORD=%s\n" % loader_password +
        "READER_PASSWORD=%s\n" % reader_password +
        "CLICKHOUSE_DSN=%s\n" % loader_dsn +
        "LIGHTSHIP_CLICKHOUSE_DSN=%s\n" % reader_dsn)
    os.chmod(temporary, stat.S_IRUSR | stat.S_IWUSR)
    os.replace(temporary, path)


def provision(database, schema_path, secrets_path, admin_dsn, resume=False):
    exists = database.query("EXISTS DATABASE %s" % DATABASE).strip() == "1"
    if exists and not resume:
        raise RuntimeError("%s already exists; inspect it and pass --resume-existing to continue without replacing it" % DATABASE)
    loader_password, reader_password = credentials(secrets_path)
    for statement in schema_statements(schema_path):
        database.query(statement)
    database.query("CREATE USER IF NOT EXISTS %s IDENTIFIED BY %s" %
                   (LOADER, sql_string(loader_password)))
    database.query("CREATE USER IF NOT EXISTS %s IDENTIFIED BY %s" %
                   (READER, sql_string(reader_password)))
    database.query("ALTER USER %s SETTINGS %s" % (READER, READER_SETTINGS))
    database.query("GRANT CREATE TABLE, DROP TABLE, INSERT, SELECT, ALTER TABLE "
                   "ON lightship_demo.* TO %s" % LOADER)
    database.query("GRANT SELECT ON lightship_demo.agent_traces TO %s" % READER)
    write_credentials(secrets_path, admin_dsn, loader_password, reader_password)


def main(argv=None, database=None, admin_dsn=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--schema", type=Path, default=DIRECTORY / "schema.sql")
    parser.add_argument("--secrets-file", type=Path, default=DEFAULT_SECRETS)
    parser.add_argument("--resume-existing", action="store_true")
    args = parser.parse_args(argv)
    admin_dsn = admin_dsn or os.environ.get("CLICKHOUSE_DSN")
    if not admin_dsn:
        parser.error("CLICKHOUSE_DSN is required for one-time provisioning")
    database = database or ClickHouse.from_dsn(admin_dsn)
    provision(database, args.schema, args.secrets_file, admin_dsn, args.resume_existing)
    print("provisioned %s; scoped credentials saved to %s" % (DATABASE, args.secrets_file))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, RuntimeError, ValueError) as error:
        print("provisioning failed: %s" % error, file=sys.stderr)
        sys.exit(1)
