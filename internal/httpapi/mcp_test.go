package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lightshipHQ/lightship/internal/store"
)

// stubCall stands in for a tools/call dispatcher so the transport can be tested without a store
// or a ClickHouse behind it. It echoes the request id, which is what the batch tests need to
// prove ordering.
func stubCall(_ *http.Request, req rpcRequest) rpcResponse {
	return okResponse(req.ID, map[string]any{"called": true})
}

// postRPC runs one HTTP POST through the rpc dispatcher and returns the recorder.
func postRPC(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	instructions := analysisServerInstructions + "\n\nFor this connection, the REST trace download " +
		"endpoint is " + restQueryURL(r) + ". Use the same bearer credential."
	s.rpc(rec, r, "lightship", instructions, s.mcpTools(), stubCall)
	return rec
}

// TestMuxMethodNotAllowed is the empirical record of what Go's ServeMux does for a method
// mismatch, and why lightship still needs an explicit GET route. A path registered under one
// method natively yields 405 with an Allow header for other methods — but only when no other
// pattern fully matches. LightShip registers "GET /" for the UI, which fully matches GET /mcp
// and swallows the probe with a 404, so the 405 has to be registered by hand.
func TestMuxMethodNotAllowed(t *testing.T) {
	t.Run("native 405 without a catch-all", func(t *testing.T) {
		m := http.NewServeMux()
		m.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {})
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
			t.Fatalf("got %d Allow=%q, want 405 Allow=POST", rec.Code, rec.Header().Get("Allow"))
		}
	})

	t.Run("the GET / catch-all defeats the native 405", func(t *testing.T) {
		m := http.NewServeMux()
		m.HandleFunc("GET /", http.NotFound)
		m.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {})
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404 — if this changed, the explicit GET routes may be redundant", rec.Code)
		}
	})

	t.Run("explicit GET route restores 405", func(t *testing.T) {
		// The same registration shape server.go uses once the GET route is in place.
		m := http.NewServeMux()
		m.HandleFunc("GET /", http.NotFound)
		m.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {})
		m.HandleFunc("GET /mcp", mcpGet)
		m.HandleFunc("POST /mcp/setup", func(w http.ResponseWriter, r *http.Request) {})
		m.HandleFunc("GET /mcp/setup", mcpGet)

		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
			t.Fatalf("GET /mcp: got %d Allow=%q, want 405 Allow=POST",
				rec.Code, rec.Header().Get("Allow"))
		}
		// Any other method now falls to the mux's native 405, because two method-specific
		// patterns claim the path and neither matches.
		rec = httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest("DELETE", "/mcp", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE /mcp: got %d, want 405", rec.Code)
		}
		rec = httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp/setup", nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
			t.Fatalf("GET /mcp/setup: got %d Allow=%q, want 405 Allow=POST",
				rec.Code, rec.Header().Get("Allow"))
		}
	})
}

func TestRPCSingle(t *testing.T) {
	t.Run("single request unchanged", func(t *testing.T) {
		rec := postRPC(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q", ct)
		}
		// A single request must answer with a single object, never a one-element array.
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body is not a single object: %v: %s", err, rec.Body.String())
		}
		if resp["id"] != float64(1) || resp["error"] != nil {
			t.Fatalf("unexpected response: %v", resp)
		}
	})

	t.Run("single notification gets 202 and no body", func(t *testing.T) {
		rec := postRPC(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
			t.Fatalf("got %d with body %q, want 202 empty", rec.Code, rec.Body.String())
		}
	})

	t.Run("malformed body is a parse error", func(t *testing.T) {
		rec := postRPC(t, `{"jsonrpc":`)
		var resp rpcResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Error == nil || resp.Error.Code != -32700 {
			t.Fatalf("want -32700, got %+v", resp.Error)
		}
	})

	t.Run("id without method is invalid request", func(t *testing.T) {
		rec := postRPC(t, `{"jsonrpc":"2.0","id":7}`)
		var resp rpcResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Error == nil || resp.Error.Code != -32600 {
			t.Fatalf("want -32600, got %+v", resp.Error)
		}
	})
}

func TestRPCBatch(t *testing.T) {
	// errCode pulls the error code out of one decoded response entry, 0 when it is a success.
	errCode := func(entry map[string]any) int {
		e, ok := entry["error"].(map[string]any)
		if !ok {
			return 0
		}
		c, _ := e["code"].(float64)
		return int(c)
	}

	tests := []struct {
		name string
		body string
		// wantIDs and wantCodes describe the expected response array, in order. A nil id is the
		// JSON null the spec assigns to an unidentifiable request.
		wantIDs   []any
		wantCodes []int
	}{
		{
			name: "mixed requests and notifications keep order and drop notifications",
			body: `[{"jsonrpc":"2.0","id":1,"method":"ping"},
			        {"jsonrpc":"2.0","method":"notifications/initialized"},
			        {"jsonrpc":"2.0","id":"two","method":"tools/list"},
			        {"jsonrpc":"2.0","method":"notifications/cancelled"},
			        {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x"}}]`,
			wantIDs:   []any{float64(1), "two", float64(3)},
			wantCodes: []int{0, 0, 0},
		},
		{
			name:      "malformed element yields an entry with a null id",
			body:      `[42, {"jsonrpc":"2.0","id":1,"method":"ping"}, "nonsense"]`,
			wantIDs:   []any{nil, float64(1), nil},
			wantCodes: []int{-32600, 0, -32600},
		},
		{
			name:      "unknown method in a batch answers in place",
			body:      `[{"jsonrpc":"2.0","id":1,"method":"no/such"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`,
			wantIDs:   []any{float64(1), float64(2)},
			wantCodes: []int{-32601, 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postRPC(t, tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var out []map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("body is not an array: %v: %s", err, rec.Body.String())
			}
			if len(out) != len(tt.wantIDs) {
				t.Fatalf("got %d responses, want %d: %s", len(out), len(tt.wantIDs), rec.Body.String())
			}
			for i := range out {
				if out[i]["id"] != tt.wantIDs[i] {
					t.Errorf("response %d: id = %v, want %v", i, out[i]["id"], tt.wantIDs[i])
				}
				if got := errCode(out[i]); got != tt.wantCodes[i] {
					t.Errorf("response %d: error code = %d, want %d", i, got, tt.wantCodes[i])
				}
			}
		})
	}

	t.Run("all-notification batch gets 202 and no body", func(t *testing.T) {
		rec := postRPC(t, `[{"jsonrpc":"2.0","method":"notifications/initialized"},
		                    {"jsonrpc":"2.0","method":"notifications/cancelled"}]`)
		if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
			t.Fatalf("got %d with body %q, want 202 empty", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty array is a single invalid-request error", func(t *testing.T) {
		rec := postRPC(t, `[]`)
		var resp rpcResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body is not a single object: %v: %s", err, rec.Body.String())
		}
		if resp.Error == nil || resp.Error.Code != -32600 {
			t.Fatalf("want -32600, got %+v", resp.Error)
		}
	})

	t.Run("unterminated array is a parse error", func(t *testing.T) {
		rec := postRPC(t, `[{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		var resp rpcResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Error == nil || resp.Error.Code != -32700 {
			t.Fatalf("want -32700, got %+v", resp.Error)
		}
	})
}

// TestInitializeVersionNegotiation covers the one initialize behavior a real client depends on:
// a client speaking an older revision this server can serve identically must have that revision
// echoed back, or it will disconnect believing the server incompatible.
func TestInitializeVersionNegotiation(t *testing.T) {
	tests := []struct {
		name, requested, want string
	}{
		{"current version echoed", mcpProtocolVersion, mcpProtocolVersion},
		{"older 2025-03-26 echoed", "2025-03-26", "2025-03-26"},
		{"older 2024-11-05 echoed", "2024-11-05", "2024-11-05"},
		{"unknown version answered with ours", "1999-01-01", mcpProtocolVersion},
		{"missing version answered with ours", "", mcpProtocolVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := "{}"
			if tt.requested != "" {
				params = `{"protocolVersion":"` + tt.requested + `"}`
			}
			rec := postRPC(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+params+`}`)
			var resp struct {
				Result struct {
					ProtocolVersion string `json:"protocolVersion"`
				} `json:"result"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Result.ProtocolVersion != tt.want {
				t.Fatalf("got %q, want %q", resp.Result.ProtocolVersion, tt.want)
			}
		})
	}
}

func TestInitializeAdvertisesDocumentationAndRESTHandoff(t *testing.T) {
	rec := postRPC(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	var resp struct {
		Result struct {
			Instructions string `json:"instructions"`
			ServerInfo   struct {
				WebsiteURL string `json:"websiteUrl"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result.ServerInfo.WebsiteURL != mcpDocsURL {
		t.Fatalf("websiteUrl = %q, want %q", resp.Result.ServerInfo.WebsiteURL, mcpDocsURL)
	}
	for _, want := range []string{mcpDocsURL, "http://example.com/traces/query",
		"search client configuration or credential files"} {
		if !strings.Contains(resp.Result.Instructions, want) {
			t.Fatalf("instructions do not contain %q: %s", want, resp.Result.Instructions)
		}
	}
}

func TestInitializeIdentifiesSeparatedSurfaces(t *testing.T) {
	decode := func(rec *httptest.ResponseRecorder) (string, string) {
		t.Helper()
		var resp struct {
			Result struct {
				Instructions string `json:"instructions"`
				ServerInfo   struct {
					Name string `json:"name"`
				} `json:"serverInfo"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Result.ServerInfo.Name, resp.Result.Instructions
	}
	analysisName, analysisInstructions := decode(postAnalysis(t, []string{store.AdminRole},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	setupName, setupInstructions := decode(postSetup(t, []string{store.AdminRole},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if analysisName != "lightship" || strings.Contains(analysisInstructions, "manage users") {
		t.Fatalf("analysis identity crossed into setup: %q %q", analysisName, analysisInstructions)
	}
	if setupName != "lightship-setup" || !strings.Contains(setupInstructions, "manage users") ||
		strings.Contains(setupInstructions, "/traces/query") {
		t.Fatalf("setup identity is not isolated: %q %q", setupName, setupInstructions)
	}
}

func TestRESTQueryURLUsesPublicRequestOrigin(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://lightship.internal/mcp", nil)
	r.Host = "traces.example.com"
	r.Header.Set("X-Forwarded-Proto", "https")
	if got, want := restQueryURL(r), "https://traces.example.com/traces/query"; got != want {
		t.Fatalf("restQueryURL = %q, want %q", got, want)
	}
	// Ignore arbitrary forwarded values instead of turning them into a URL scheme.
	r.Header.Set("X-Forwarded-Proto", "javascript")
	if got, want := restQueryURL(r), "http://traces.example.com/traces/query"; got != want {
		t.Fatalf("restQueryURL with invalid scheme = %q, want %q", got, want)
	}
}

func TestMCPQueryResultsAreContextBounded(t *testing.T) {
	t.Run("broad summary request is refused before querying", func(t *testing.T) {
		rec := postAnalysis(t, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
			`"params":{"name":"lightship_search_traces","arguments":{"limit":200}}}`)
		body := rec.Body.String()
		if !strings.Contains(body, `"isError":true`) ||
			!strings.Contains(body, "accept limit 1-20") ||
			!strings.Contains(body, "http://example.com/traces/query") {
			t.Fatalf("unexpected broad-query response: %s", body)
		}
	})

	t.Run("oversized query result is replaced by a handoff", func(t *testing.T) {
		resp := boundedQueryToolResult(json.RawMessage(`1`),
			map[string]any{"spans": strings.Repeat("x", mcpQueryResultMaxSize)}, false,
			"https://traces.example.com/traces/query")
		body, err := json.Marshal(resp.Result)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"isError":true`) ||
			!strings.Contains(string(body), "result is too large for MCP context") ||
			!strings.Contains(string(body), "https://traces.example.com/traces/query") {
			t.Fatalf("unexpected oversized-query response: %s", body)
		}
	})
}

func TestQueryToolsAdvertiseDocumentationAndSummaryLimit(t *testing.T) {
	tools := (&Server{}).mcpTools()
	for _, tool := range tools {
		description, _ := tool["description"].(string)
		if !strings.Contains(description, mcpDocsURL) {
			t.Errorf("tool %q does not link its documentation", tool["name"])
		}
		if tool["name"] != "lightship_search_traces" {
			continue
		}
		input := tool["inputSchema"].(map[string]any)
		properties := input["properties"].(map[string]any)
		limit := properties["limit"].(map[string]any)
		if limit["maximum"] != mcpSummaryLimit || limit["default"] != mcpSummaryDefault {
			t.Fatalf("search limit schema = %#v", limit)
		}
	}
}

// postAnalysis and postSetup exercise the two MCP surfaces after authentication has resolved a
// session. The integration suite covers the route middleware and real REST dispatch.
func postAnalysis(t *testing.T, roles []string, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	sess := &store.Session{Username: "u", Roles: roles}
	s.analysisMCP(rec, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	return rec
}

func postSetup(t *testing.T, roles []string, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/mcp/setup", strings.NewReader(body))
	sess := &store.Session{Username: "u", Roles: roles}
	s.setupMCP(rec, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	return rec
}

func TestSeparatedToolLists(t *testing.T) {
	names := func(post func(*testing.T, []string, string) *httptest.ResponseRecorder,
		roles []string) map[string]bool {
		t.Helper()
		rec := post(t, roles, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		var resp struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, tl := range resp.Result.Tools {
			out[tl.Name] = true
		}
		return out
	}

	plain := names(postAnalysis, nil)
	adminAnalysis := names(postAnalysis, []string{store.AdminRole})
	setup := names(postSetup, []string{store.AdminRole})

	for _, want := range []string{"lightship_filter_schema", "lightship_search_traces",
		"lightship_get_trace"} {
		if !plain[want] || !adminAnalysis[want] || setup[want] {
			t.Errorf("query tool %s must be listed for every caller", want)
		}
	}
	for _, tool := range adminTools() {
		if plain[tool.Name] || adminAnalysis[tool.Name] || !setup[tool.Name] {
			t.Errorf("setup tool %s is not isolated to the setup server", tool.Name)
		}
	}
	// The key tools are not admin tools. Minting one's own key is the only way any principal gets
	// a credential now that nobody can mint on another's behalf, so a non-admin who cannot reach
	// them can sign in and then do nothing.
	for _, tool := range selfTools() {
		if !plain[tool.Name] || !adminAnalysis[tool.Name] || setup[tool.Name] {
			t.Errorf("self-service tool %s must be listed for every caller", tool.Name)
		}
	}
}

// TestSelfToolAllowsNonAdmin is the other half of the control: the admin check keys off the tool
// set, not the route, so a non-admin naming a key tool is let through the gate rather than refused.
//
// It asks to revoke a key without saying which, so the call fails on its own arguments while still
// inside the tool layer. That is the assertion: reaching an argument error proves the admin gate
// let a non-admin past, without needing the store this harness does not build.
func TestSelfToolAllowsNonAdmin(t *testing.T) {
	rec := postAnalysis(t, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"lightship_revoke_key","arguments":{}}}`)
	body := rec.Body.String()
	if strings.Contains(body, "admin only") {
		t.Fatalf("a non-admin must reach the key tools: %s", body)
	}
	if !strings.Contains(body, "id is required") {
		t.Fatalf("want the tool's own argument error, got %s", body)
	}
}

func TestToolCallsCannotCrossMCPSurfaces(t *testing.T) {
	// Even an administrator cannot reach setup by naming an undisclosed tool on the analysis MCP.
	rec := postAnalysis(t, []string{store.AdminRole}, `{"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"lightship_delete_user","arguments":{"username":"x"}}}`)
	if !strings.Contains(rec.Body.String(), "unknown tool") {
		t.Fatalf("analysis MCP accepted a setup tool: %s", rec.Body.String())
	}

	// The setup dispatcher repeats the role check even though the HTTP route is already adminOnly.
	rec = postSetup(t, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"lightship_delete_user","arguments":{"username":"x"}}}`)
	if !strings.Contains(rec.Body.String(), "admin only") {
		t.Fatalf("setup MCP did not repeat its admin check: %s", rec.Body.String())
	}

	// Setup never dispatches analysis tools, including for an administrator.
	rec = postSetup(t, []string{store.AdminRole}, `{"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"lightship_search_traces","arguments":{}}}`)
	if !strings.Contains(rec.Body.String(), "unknown tool") {
		t.Fatalf("setup MCP accepted an analysis tool: %s", rec.Body.String())
	}
}

// TestCreateUserToolMatchesTheRoute pins the provisioning tool to the contract POST /users actually
// implements. An agent works from the schema alone, so a tool that cannot express
// must_change_password would quietly enrol every service account into a change it will never
// perform — and one that demanded password_hash would push the agent back to computing hashes,
// which is the thing generation exists to stop.
func TestCreateUserToolMatchesTheRoute(t *testing.T) {
	var tool *adminTool
	for _, tl := range adminTools() {
		if tl.Name == "lightship_create_user" {
			tool = &tl
			break
		}
	}
	if tool == nil {
		t.Fatal("lightship_create_user is missing from the admin tools")
	}
	props, _ := tool.Schema["properties"].(map[string]any)
	if _, ok := props["must_change_password"]; !ok {
		t.Error("the tool cannot express must_change_password")
	}
	required, _ := tool.Schema["required"].([]string)
	for _, r := range required {
		if r == "password_hash" {
			t.Error("password_hash is optional on the route and must be optional here")
		}
	}
	if !strings.Contains(tool.Desc, "must_change_password") {
		t.Error("the description must tell an agent what the flag does")
	}
}
