package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/lightshipHQ/lightship/internal/filter"
	"github.com/lightshipHQ/lightship/internal/traces"
)

// The analysis MCP surface, speaking JSON-RPC 2.0 over HTTP.
//
// It is deliberately thin. Every tool goes through the same functions the REST handlers do, so
// there is one query path — a second would be how a policy filter gets forgotten, which is the
// failure this whole product exists to prevent. Authentication is the same too: an MCP client
// presents a bearer API key, which is why keys became self-service.
//
// What is NOT here is the point. Every comparable tool's MCP server has a raw-SQL escape hatch —
// ClickStack's `clickstack_sql` — and it is the one shape we can never adopt: raw SQL bypasses the
// policy. So the constrained surface has to be good enough on its own, and an agent has to be able
// to discover it, which is what lightship_filter_schema is for.

const (
	mcpProtocolVersion    = "2025-06-18"
	mcpDocsURL            = "https://lightship.mintlifysite.com/docs"
	mcpSummaryDefault     = 10
	mcpSummaryLimit       = 20
	mcpQueryResultMaxSize = 32 << 10
)

// analysisServerInstructions is returned from initialize and surfaced to the model as guidance about the
// server as a whole. It exists because the most important thing to know here is not about any one
// tool: bulk trace rows must not come through this transport at all.
const analysisServerInstructions = `LightShip serves OpenTelemetry traces under an access policy. Every
read is already restricted to what your credential permits, so you never need to filter results
yourself.

These tools are for reasoning: what can be filtered, which traces exist, what one trace looks like.
They are not for bulk data. A trace with realistic LLM payloads costs thousands of tokens, so a page
of them would consume most of a context window.

To analyse more than a trace or two, fetch rows over HTTP and work on the file. POST to
<base>/traces/query, where <base> is the origin this MCP endpoint is served from, with the same
bearer credential:

    {"from": "...", "to": "...", "limit": 200,
     "columns": ["TraceId", "SpanId", "Timestamp", "SpanAttributes"]}

The response is {binding, spans, next_cursor}. Write it to a file, follow next_cursor for the next
page, and then grep, jq or DuckDB the file — none of it needs to enter context. limit counts traces,
not spans. Omit columns and every column comes back, which on a table carrying prompts and
completions is a great deal of text.

When the local LightShip MCP companion is installed, use lightship_export_traces for bulk analysis.
It writes the authorized rows into the workspace and returns only a path and counts, so the trace
payload does not enter model context. Otherwise, the initialize response and
lightship_filter_schema provide the concrete REST URL. If the client does not expose its bearer
credential to the local HTTP client, ask the operator for a safe way to run the download. Do not
search client configuration or credential files.

Call lightship_filter_schema first: it lists every field a filter may name, and it is complete, so a
filter naming anything else will be refused.

Documentation: https://lightship.mintlifysite.com/docs`

const setupServerInstructions = `This is LightShip Setup, the administrator-only control plane.
Use it to connect the trace source, select fields, define roles, manage users, and inspect the
resulting configuration. Present discovered choices before binding a table, and ask the operator
which fields, attributes, roles, and users they intend rather than inventing access decisions.
This server does not expose trace-query tools; use the regular LightShip MCP connection for
analysis.

Documentation: https://lightship.mintlifysite.com/docs`

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// analysisMCP is the everyday surface. It is intentionally identical for administrators and other
// users: connecting the analysis server can never put a configuration mutation in an agent's tool
// inventory. Self-service key tools remain here until the local companion replaces that workflow.
func (s *Server) analysisMCP(w http.ResponseWriter, r *http.Request) {
	tools := append(s.mcpTools(), selfToolList()...)
	s.rpc(w, r, "lightship", analysisServerInstructions+"\n\nFor this connection, the REST trace download "+
		"endpoint is "+restQueryURL(r)+". Use the same bearer credential.", tools, s.analysisCall)
}

// setupMCP is a separate server rather than a role-dependent presentation of the analysis server.
// The route is adminOnly, and setupCall repeats the check for direct invocation and future reuse.
func (s *Server) setupMCP(w http.ResponseWriter, r *http.Request) {
	s.rpc(w, r, "lightship-setup", setupServerInstructions, adminToolList(), s.setupCall)
}

// mcpGet answers a GET probe of an MCP endpoint. The spec wants 405 from a server that offers no
// server-initiated SSE stream; a 404 would instead tell the client the endpoint does not exist.
// The mux would produce this 405 itself for a path registered only under POST, but the GET /
// catch-all fully matches these paths and swallows the probe, so server.go registers this
// explicitly. It answers before authentication on purpose: a 405 discloses nothing the README
// does not, and a client probing capabilities may not send credentials yet.
func mcpGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	w.WriteHeader(http.StatusMethodNotAllowed)
}

// rpc handles one HTTP POST carrying either a single JSON-RPC message or a batch — an array of
// them — for a given tool set. Notifications carry no id and get no response, per the spec.
func (s *Server) rpc(w http.ResponseWriter, r *http.Request, name, instructions string, tools []map[string]any,
	call func(*http.Request, rpcRequest) rpcResponse) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		writeOne(w, errResponse(nil, -32700, "parse error"))
		return
	}
	// A leading '[' is the one dispatch the two body shapes allow: a batch is an array at the
	// top level and nothing else is.
	if t := bytes.TrimLeft(body, " \t\r\n"); len(t) > 0 && t[0] == '[' {
		s.rpcBatch(w, r, t, name, instructions, tools, call)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOne(w, errResponse(nil, -32700, "parse error"))
		return
	}
	resp := s.rpcOne(r, name, instructions, tools, call, req)
	if resp == nil {
		// A notification. Nothing to answer, and answering would be a protocol error.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeOne(w, *resp)
}

// rpcBatch answers a JSON-RPC batch: one response per request, in request order, with
// notifications contributing no entry. An element that is not a request object at all still owes
// the caller an error entry, with a null id, because a malformed element has no usable one.
func (s *Server) rpcBatch(w http.ResponseWriter, r *http.Request, body []byte,
	name, instructions string, tools []map[string]any,
	call func(*http.Request, rpcRequest) rpcResponse) {
	var elems []json.RawMessage
	if err := json.Unmarshal(body, &elems); err != nil {
		writeOne(w, errResponse(nil, -32700, "parse error"))
		return
	}
	if len(elems) == 0 {
		// The spec singles this case out: an empty array is Invalid Request, answered with a
		// single error object rather than an empty array.
		writeOne(w, errResponse(nil, -32600, "invalid request: empty batch"))
		return
	}
	out := make([]rpcResponse, 0, len(elems))
	for _, e := range elems {
		var req rpcRequest
		if err := json.Unmarshal(e, &req); err != nil {
			out = append(out, errResponse(nil, -32600, "invalid request"))
			continue
		}
		if resp := s.rpcOne(r, name, instructions, tools, call, req); resp != nil {
			out = append(out, *resp)
		}
	}
	if len(out) == 0 {
		// Every element was a notification, so there is nothing to say — the MCP transport wants
		// 202 with no body, the same answer a single notification gets.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// rpcOne dispatches a single JSON-RPC message and returns its response, or nil for a
// notification, which must get none.
func (s *Server) rpcOne(r *http.Request, name, instructions string, tools []map[string]any,
	call func(*http.Request, rpcRequest) rpcResponse, req rpcRequest) *rpcResponse {
	if len(req.ID) == 0 {
		return nil
	}
	var resp rpcResponse
	switch req.Method {
	case "initialize":
		// Version negotiation: when the client asked for a revision this server can serve
		// identically — it is stateless JSON-over-POST, which every revision listed here
		// permits — echo that revision back so an older client proceeds instead of
		// disconnecting. Anything else gets our own version, and the client decides.
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := mcpProtocolVersion
		switch p.ProtocolVersion {
		case "2024-11-05", "2025-03-26":
			version = p.ProtocolVersion
		}
		resp = okResponse(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name": name, "version": "1", "websiteUrl": mcpDocsURL,
			},
			// The spec's channel for telling a client how the server is meant to be used. It is
			// where the division of labour goes, because no individual tool can state it.
			"instructions": instructions,
		})
	case "ping":
		resp = okResponse(req.ID, map[string]any{})
	case "tools/list":
		resp = okResponse(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		resp = call(r, req)
	case "":
		// An id with no method is not a request at all; -32601 would claim the method merely
		// does not exist.
		resp = errResponse(req.ID, -32600, "invalid request")
	default:
		resp = errResponse(req.ID, -32601, "unknown method "+req.Method)
	}
	return &resp
}

func okResponse(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errResponse(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func writeOne(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC carries its own errors; HTTP status is transport only
	_ = json.NewEncoder(w).Encode(resp)
}

// filterSchemaProperty is repeated in both search tools because MCP has no shared-schema mechanism
// an arbitrary client is guaranteed to follow.
var filterProperty = map[string]any{
	"type": "object",
	"description": "Conditions are ANDed. A trace matches when ANY of its spans satisfies them. " +
		"Call lightship_filter_schema first: only fields listed there may be named.",
	"properties": map[string]any{
		"conditions": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"map":  map[string]any{"type": "string", "description": "the map column, omitted for a scalar column"},
					"name": map[string]any{"type": "string", "description": "the column name, or the key within map"},
					"op":   map[string]any{"type": "string", "enum": filter.Operators()},
					"value": map[string]any{"description": "typed value matching the field", "anyOf": []map[string]any{
						{"type": "string"}, {"type": "number"}, {"type": "boolean"},
					}},
					"values": map[string]any{"type": "array", "items": map[string]any{"anyOf": []map[string]any{
						{"type": "string"}, {"type": "number"}, {"type": "boolean"},
					}}},
				},
				"required": []string{"name", "op"},
			},
		},
	},
}

func (s *Server) mcpTools() []map[string]any {
	return []map[string]any{
		{
			"name": "lightship_filter_schema",
			"description": "What you may filter on, and with which operators. The set is declared " +
				"rather than inferred, so it is complete: no other field can be named in a filter. " +
				"Call this before searching. Documentation: " + mcpDocsURL + "/api-reference/filters",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name": "lightship_search_traces",
			"description": "Summaries of traces in a time window, optionally filtered — trace id, " +
				"start, duration, root operation, service, span count. Results are restricted to " +
				"what your roles permit; a filter can only narrow that further.\n\n" +
				fmt.Sprintf("This returns at most %d summaries, not rows. Larger requests are refused ", mcpSummaryLimit) +
				"so trace listings cannot consume the context window. For the spans themselves — and for anything " +
				"more than a trace or two — POST to <base>/traces/query with the same " +
				"{filter, from, to, limit, cursor} plus optional columns, and write the response to " +
				"a file rather than reading it into context. <base> is the origin this MCP endpoint " +
				"is served from. Documentation: " + mcpDocsURL + "/use/rest-download",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from": map[string]any{"type": "string", "description": "RFC3339; defaults to 24h ago"},
					"to":   map[string]any{"type": "string", "description": "RFC3339; defaults to now"},
					"limit": map[string]any{"type": "integer", "minimum": 1,
						"maximum": mcpSummaryLimit, "default": mcpSummaryDefault,
						"description": "summary count; use REST for larger result sets"},
					"cursor": map[string]any{"type": "string", "description": "next_cursor from a previous call; only valid with the same filter"},
					"filter": filterProperty,
				},
			},
		},
		{
			"name": "lightship_get_trace",
			"description": "Every span of one trace, as stored. Returns not-found both when the " +
				"trace does not exist and when your roles do not permit it — the two are " +
				"deliberately indistinguishable.\n\n" +
				"Use columns to project: without it every column comes back, which on a table " +
				"carrying prompts and completions is a great deal of text. For more than one or " +
				"two traces, fetch over HTTP to a file instead — see the server instructions. " +
				"Documentation: " + mcpDocsURL + "/api-reference/traces/get",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"trace_id": map[string]any{"type": "string"},
					"columns": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
						"description": "columns to return; omit for every column, which on a table " +
							"carrying payload attributes is a great deal of text"},
				},
				"required": []string{"trace_id"},
			},
		},
	}
}

func (s *Server) analysisCall(r *http.Request, req rpcRequest) rpcResponse {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			From    string            `json:"from"`
			To      string            `json:"to"`
			Limit   int               `json:"limit"`
			Cursor  string            `json:"cursor"`
			Filter  filter.Filter     `json:"filter"`
			TraceID string            `json:"trace_id"`
			Columns traces.Projection `json:"columns"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResponse(req.ID, -32602, "invalid params: "+err.Error())
	}
	a := p.Arguments

	switch p.Name {
	case "lightship_filter_schema":
		m, err := s.models.Get(r.Context())
		if err != nil {
			return toolResult(req.ID, map[string]any{"error": err.Error()}, true)
		}
		body := s.filterSchemaBody(r, m)
		body["rest_query_url"] = restQueryURL(r)
		body["documentation_url"] = mcpDocsURL + "/api-reference/filters"
		return boundedQueryToolResult(req.ID, body, false, restQueryURL(r))

	case "lightship_search_traces":
		win, err := windowFrom(a.From, a.To)
		if err != nil {
			return toolResult(req.ID, map[string]any{"error": err.Error()}, true)
		}
		if err := s.checkWindow(win); err != nil {
			return toolResult(req.ID, map[string]any{"error": err.Error()}, true)
		}
		limit := a.Limit
		if limit < 0 || limit > mcpSummaryLimit {
			return toolResult(req.ID, map[string]any{
				"error":             fmt.Sprintf("MCP trace summaries accept limit 1-%d; use %s to download larger result sets to a file", mcpSummaryLimit, restQueryURL(r)),
				"documentation_url": mcpDocsURL + "/use/rest-download",
			}, true)
		}
		if limit == 0 {
			limit = mcpSummaryDefault
		}
		body, code := s.listResult(r, win, a.Filter, limit, a.Cursor)
		return boundedQueryToolResult(req.ID, body, code != http.StatusOK, restQueryURL(r))

	case "lightship_get_trace":
		if a.TraceID == "" {
			return toolResult(req.ID, map[string]any{"error": "trace_id is required"}, true)
		}
		body, code := s.detailResult(r, a.TraceID, a.Columns)
		return boundedQueryToolResult(req.ID, body, code != http.StatusOK, restQueryURL(r))

	default:
		return s.selfCall(r, req)
	}
}

func restQueryURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	return (&url.URL{Scheme: scheme, Host: r.Host, Path: "/traces/query"}).String()
}

// boundedQueryToolResult keeps trace data from becoming an accidental bulk transport. REST has
// pagination and projection for larger responses; MCP results go directly into model context.
func boundedQueryToolResult(id json.RawMessage, body any, isError bool, restURL string) rpcResponse {
	encoded, err := json.Marshal(body)
	if err == nil && len(encoded) > mcpQueryResultMaxSize {
		return toolResult(id, map[string]any{
			"error":             fmt.Sprintf("result is too large for MCP context (maximum %d bytes); use %s with projection and pagination to write it to a file", mcpQueryResultMaxSize, restURL),
			"documentation_url": mcpDocsURL + "/use/rest-download",
		}, true)
	}
	return toolResult(id, body, isError)
}

// toolResult renders a tool's output. isError marks a failure the model should read and react
// to, rather than a protocol error — a denied read is a normal outcome here, not a fault.
func toolResult(id json.RawMessage, body any, isError bool) rpcResponse {
	// Compact, not indented. A tool result is a conversation turn, and whitespace a model has to
	// read is context spent on nothing.
	text, err := json.Marshal(body)
	if err != nil {
		text = []byte(`{"error":"could not render result"}`)
	}
	return okResponse(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(text)}},
		"isError": isError,
	})
}
