"""Access-model and pagination tests for retail demo configuration."""

import unittest

import configure


class RecordingAdmin:
    def __init__(self, existing=()):
        self.existing = existing
        self.calls = []

    def request(self, method, path, body=None, expected=200, preview_role=None):
        self.calls.append((method, path, body, expected))
        if method == "GET" and path == "/users":
            return {"users": [{"username": name} for name in self.existing]}
        return {}


class PagedClient:
    def __init__(self):
        self.requests = []

    def request(self, method, path, body=None, expected=200, preview_role=None):
        self.requests.append(body)
        if body.get("cursor") == "second":
            return {"spans": [{"TraceId": "trace-2"}]}
        return {"spans": [{"TraceId": "trace-3"}], "next_cursor": "second"}


class ConfigureTests(unittest.TestCase):
    def test_configures_tenant_scoped_preview_role_without_demo_users(self):
        admin = RecordingAdmin()
        configure.configure(admin)
        roles = {path.removeprefix("/roles/"): body["policies"][0]["expression"]
                 for method, path, body, expected in admin.calls
                 if method == "PUT" and path.startswith("/roles/")}
        self.assertEqual(roles, {
            "cedar-account-manager": 'TenantId == "cedar" && AgentName == "support-agent"',
            "northstar-account-manager":
                'TenantId == "northstar" && AgentName == "support-agent"',
            "support-operations": 'AgentName == "support-agent"',
        })
        self.assertFalse(any(method == "POST" and path == "/users"
                             for method, path, _, _ in admin.calls))
        fields = next(body["fields"] for method, path, body, _ in admin.calls
                      if method == "PUT" and path == "/schema/fields")
        trace_id = next(field for field in fields if field["name"] == "TraceId")
        self.assertTrue(trace_id["filterable"])
        self.assertFalse(trace_id["policy"])

    def test_pagination_keeps_fixed_bounds_and_returns_every_trace(self):
        client = PagedClient()
        window = {"from": "2026-09-08T00:00:00Z", "to": "2026-09-09T00:00:00Z"}
        self.assertEqual(configure.paged_trace_ids(client, window), ["trace-3", "trace-2"])
        self.assertEqual(client.requests[1]["cursor"], "second")
        self.assertEqual(client.requests[0]["from"], client.requests[1]["from"])
        self.assertEqual(client.requests[0]["to"], client.requests[1]["to"])


if __name__ == "__main__":
    unittest.main()
