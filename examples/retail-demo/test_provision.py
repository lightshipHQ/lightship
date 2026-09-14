"""Safety tests for hosted ClickHouse provisioning."""

import os
from pathlib import Path
import tempfile
import unittest

import provision


class FakeClickHouse:
    def __init__(self, exists=False):
        self.exists = exists
        self.queries = []

    def query(self, sql, data=None):
        self.queries.append(sql)
        if sql.startswith("EXISTS DATABASE"):
            return "1" if self.exists else "0"
        return ""


class ProvisionTests(unittest.TestCase):
    def test_existing_generated_file_can_be_resumed(self):
        database = FakeClickHouse(exists=True)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "credentials.env"
            provision.write_credentials(
                path, "https://admin:secret@example.test:8443/otel", "loader-secret", "reader-secret")
            provision.provision(database, provision.DIRECTORY / "schema.sql", path,
                                "https://admin:secret@example.test:8443/otel", resume=True)
        self.assertTrue(any(query.startswith("ALTER USER lightship_demo_reader SETTINGS")
                            for query in database.queries))

    def test_existing_database_requires_explicit_resume(self):
        database = FakeClickHouse(exists=True)
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(RuntimeError, "--resume-existing"):
                provision.provision(database, provision.DIRECTORY / "schema.sql",
                                    Path(directory) / "credentials.env",
                                    "https://admin:secret@example.test:8443/otel", resume=False)
        self.assertEqual(len(database.queries), 1)

    def test_new_database_gets_separate_scoped_identities_and_private_file(self):
        database = FakeClickHouse()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "credentials.env"
            provision.provision(database, provision.DIRECTORY / "schema.sql", path,
                                "https://admin:secret@example.test:8443/otel")
            contents = path.read_text()
            self.assertIn("LIGHTSHIP_DEMO_CLICKHOUSE_USER=lightship_demo_loader", contents)
            self.assertIn("LIGHTSHIP_CLICKHOUSE_DSN=https://lightship_demo_reader:", contents)
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
            self.assertTrue(any("GRANT SELECT ON lightship_demo.agent_traces" in query
                                for query in database.queries))
            settings = next(query for query in database.queries
                            if query.startswith("ALTER USER lightship_demo_reader SETTINGS"))
            self.assertIn("readonly = 2", settings)
            self.assertIn("max_execution_time = 35 MAX 35", settings)
            self.assertIn("max_result_bytes = 67108864", settings)
            self.assertIn("result_overflow_mode = 'throw'", settings)
            self.assertFalse(any("DROP DATABASE" in query or "TRUNCATE" in query
                                 for query in database.queries))


if __name__ == "__main__":
    unittest.main()
