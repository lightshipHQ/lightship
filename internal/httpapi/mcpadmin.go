package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
)

// The setup MCP tools, served from the administrator-only /mcp/setup endpoint.
//
// Every tool here dispatches back through this server's own router, so the operation an agent calls
// is byte-for-byte the operation curl calls: the same validation, the same compile-before-store, the
// same audit row. MCP differs in protocol and in nothing else. Re-implementing fifteen handlers to
// speak JSON-RPC would have been fifteen chances for the two to drift.
//
// The endpoint, setupCall, and dispatched REST route each enforce the administrator role. The
// repetition is intentional: moving or reusing one layer cannot quietly expose a setup mutation.

// adminTool maps one tool call onto one HTTP request against this server.
type adminTool struct {
	Name string
	Desc string
	// Method and Path describe the route. Path may contain {placeholders} filled from arguments.
	Method string
	Path   string
	// Body names the arguments forwarded as the JSON body. Empty means no body; a single "*" means
	// the whole argument object, minus anything consumed by the path; a single "=name" means that
	// one argument sent unwrapped, for a handler whose body is the value itself.
	Body []string
	// Schema is the tool's declared input, for tools/list.
	Schema map[string]any
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

var (
	str     = map[string]any{"type": "string"}
	strs    = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	anyObj  = map[string]any{"type": "object"}
	anyArr  = map[string]any{"type": "array"}
	boolean = map[string]any{"type": "boolean"}
)

func adminTools() []adminTool {
	return []adminTool{
		{
			Name: "lightship_discover", Method: "POST", Path: "/schema/discover", Body: []string{"*"},
			Desc: "Read the customer's ClickHouse and report what is there: candidate tables ranked " +
				"by how closely they match the shape of a trace table, their columns and indexes, and " +
				"a suggested role binding. Pass table, timestamp and maps to also sample attribute " +
				"keys; from and to accept the same RFC 3339 range as trace queries. Sampling returns " +
				"per-key coverage — a policy on a key some spans lack behaves " +
				"differently from one on a key they all carry. Decides nothing.",
			Schema: obj(map[string]any{
				"table": str, "timestamp": str, "maps": strs, "from": str, "to": str,
				"window": map[string]any{"type": "string", "description": `Go duration, e.g. "168h"`},
			}),
		},
		{
			Name: "lightship_get_schema", Method: "GET", Path: "/schema",
			Desc: "The stored access model — binding, fields, user attributes, roles and policies — " +
				"and whether it compiles. Commit this if you want the model reviewable in git; it is " +
				"an export, not a source of truth.",
			Schema: obj(map[string]any{}),
		},
		{
			Name: "lightship_set_binding", Method: "PUT", Path: "/schema/binding", Body: []string{"*"},
			Desc: "Bind structural roles to columns. All six fields are required: table, trace_id, " +
				"timestamp, span_id, parent_span_id, and name. Authorization groups spans by trace id; " +
				"the timestamp defines the window and cursor. Columns are checked against the table.",
			Schema: bindingSchema(),
		},
		{
			Name: "lightship_set_fields", Method: "PUT", Path: "/schema/fields", Body: []string{"*"},
			Desc: "Replace the marked set: the closed vocabulary a policy may reference and a filter " +
				"may name. Each field is either a scalar column (name) or a key in a map column " +
				"(map + name), with two independent flags. filterable lets a caller narrow their own " +
				"query and can only shrink what they see. policy admits the field to the security " +
				"model. The expensive mistake is omission. Replaces wholesale, and is rejected if it " +
				"would unmark a field a policy still names.",
			Schema: obj(map[string]any{"fields": anyArr}, "fields"),
		},
		{
			Name: "lightship_put_role", Method: "PUT", Path: "/roles/{name}", Body: []string{"policies"},
			Desc: "Create or replace a role and its policies. A policy is CEL over the marked fields, " +
				"naming ClickHouse columns directly — ResourceAttributes[\"tenant.id\"] == " +
				"user.tenant_id. Roles OR together, so adding one can only widen. Compiled before it " +
				"is stored, so an invalid policy is an error here and the running system is untouched.",
			Schema: obj(map[string]any{"name": str, "policies": anyArr}, "name", "policies"),
		},
		{
			Name: "lightship_delete_role", Method: "DELETE", Path: "/roles/{name}",
			Desc:   "Remove a role and every assignment of it. Holders lose it on their next request.",
			Schema: obj(map[string]any{"name": str}, "name"),
		},
		{
			Name: "lightship_optimizations", Method: "GET", Path: "/schema/optimizations",
			Desc: "Which ClickHouse skip indexes the marked fields want, and the DDL to add them. " +
				"LightShip holds read-only credentials and will not run it. Run this after marking " +
				"fields; without an index the trace-selection subquery reads the whole window.",
			Schema: obj(map[string]any{}),
		},
		{
			Name: "lightship_list_users", Method: "GET", Path: "/users",
			Desc:   "Every user, with their attributes and roles.",
			Schema: obj(map[string]any{}),
		},
		{
			Name: "lightship_create_user", Method: "POST", Path: "/users", Body: []string{"*"},
			Desc: "Provision a user. Omit password_hash and LightShip generates the password, returns " +
				"it once in the response and stores only its hash — hand it over and it will not be " +
				"readable again. password_hash, if you send one, is an argon2id hash and never a " +
				"password. must_change_password defaults to true: the user is refused everything but " +
				"changing their own password until they do, so the credential you read stops working " +
				"once used. Set it false only for a service account nobody signs into interactively. " +
				"Every valid attribute key becomes available to policies automatically. A user holding " +
				"no role sees nothing.",
			Schema: obj(map[string]any{
				"username": str, "password_hash": str, "attributes": anyObj, "roles": strs,
				"must_change_password": boolean,
			}, "username"),
		},
		{
			Name: "lightship_update_user", Method: "PATCH", Path: "/users/{username}",
			Body: []string{"password_hash", "roles"},
			Desc: "Set a user's password hash or roles; omit a field to leave it alone. An empty roles " +
				"list removes every role, which suspends someone without losing their audit history. " +
				"Changing the password revokes that user's sessions.",
			Schema: obj(map[string]any{"username": str, "password_hash": str, "roles": strs}, "username"),
		},
		{
			Name: "lightship_set_attributes", Method: "PATCH", Path: "/users/{username}/attributes",
			Body: []string{"=attributes"},
			Desc: "Update named attributes, leaving the rest alone; a null value removes one. This is " +
				"the most privilege-relevant call in the product — whoever sets tenant.id decides " +
				"what that caller sees.",
			Schema: obj(map[string]any{"username": str, "attributes": anyObj}, "username", "attributes"),
		},
		{
			Name: "lightship_delete_user", Method: "DELETE", Path: "/users/{username}",
			Desc: "Deprovision. Sessions and API keys cascade with the row, so access ends at the " +
				"call rather than whenever a credential would have lapsed. The audit log survives.",
			Schema: obj(map[string]any{"username": str}, "username"),
		},
	}
}

func bindingSchema() map[string]any {
	s := obj(map[string]any{
		"table": str, "trace_id": str, "timestamp": str, "span_id": str,
		"parent_span_id": str, "name": str,
	}, "table", "trace_id", "timestamp", "span_id", "parent_span_id", "name")
	s["additionalProperties"] = false
	return s
}

// selfTools are the key tools. They are offered to every authenticated caller, not only admins,
// because a key now always acts as its creator: there is no principal to escalate to, and minting
// one grants exactly what the caller's roles already grant. That also makes them the only way a
// non-admin obtains a credential at all, since an admin can no longer mint a key on anyone's
// behalf — withholding them here would leave a user able to sign in and unable to connect an agent.
// Their REST routes are authenticated-but-not-admin for the same reason, so this set matches them.
func selfTools() []adminTool {
	return []adminTool{
		{
			Name: "lightship_list_keys", Method: "GET", Path: "/keys",
			Desc:   "Every API key, with who it acts as and when it was last used. Tokens are not stored.",
			Schema: obj(map[string]any{}),
		},
		{
			Name: "lightship_create_key", Method: "POST", Path: "/keys", Body: []string{"*"},
			Desc: "Mint an API key. A key always acts as its creator — there is no way to mint one " +
				"for another principal — so it grants exactly what the caller's roles already grant, " +
				"resolved fresh on every request. The token is returned once and only its hash is " +
				"stored.",
			Schema: obj(map[string]any{
				"name":       str,
				"expires_in": map[string]any{"type": "string", "description": `Go duration, e.g. "720h"`},
			}, "name"),
		},
		{
			Name: "lightship_revoke_key", Method: "DELETE", Path: "/keys/{id}",
			Desc:   "Revoke a key. A tombstone, so the audit log can still name it.",
			Schema: obj(map[string]any{"id": str}, "id"),
		},
	}
}

// selfToolList renders the self-service tools for tools/list. Every caller's list carries them.
func selfToolList() []map[string]any {
	return renderTools(selfTools())
}

// adminToolList renders the fixed tool inventory of the administrator-only setup MCP.
func adminToolList() []map[string]any {
	return renderTools(adminTools())
}

func renderTools(tools []adminTool) []map[string]any {
	list := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		list = append(list, map[string]any{
			"name": t.Name, "description": t.Desc, "inputSchema": t.Schema,
		})
	}
	return list
}

func (s *Server) selfCall(r *http.Request, req rpcRequest) rpcResponse {
	return s.mappedToolCall(r, req, selfTools())
}

func (s *Server) setupCall(r *http.Request, req rpcRequest) rpcResponse {
	if !sessionFrom(r.Context()).IsAdmin() {
		return toolResult(req.ID, map[string]any{"error": "admin only"}, true)
	}
	return s.mappedToolCall(r, req, adminTools())
}

func (s *Server) mappedToolCall(r *http.Request, req rpcRequest, allowed []adminTool) rpcResponse {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResponse(req.ID, -32602, "invalid params: "+err.Error())
	}
	var tool *adminTool
	for i := range allowed {
		if t := allowed[i]; t.Name == p.Name {
			tool = &t
			break
		}
	}
	if tool == nil {
		return errResponse(req.ID, -32602, "unknown tool "+p.Name)
	}

	path, body, err := tool.request(p.Arguments)
	if err != nil {
		return toolResult(req.ID, map[string]any{"error": err.Error()}, true)
	}

	// Dispatch through this server's own router: the same handler, the same middleware, the same
	// audit. The request carries the original's credentials so it re-authenticates as the caller.
	inner := httptest.NewRequest(tool.Method, path, bytes.NewReader(body))
	inner.Header.Set("Content-Type", "application/json")
	if v := r.Header.Get("Authorization"); v != "" {
		inner.Header.Set("Authorization", v)
	}
	for _, c := range r.Cookies() {
		inner.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, inner.WithContext(r.Context()))

	var out any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		out = map[string]any{"error": strings.TrimSpace(rec.Body.String())}
	}
	return toolResult(req.ID, out, rec.Code >= 400)
}

// request renders a tool call as a path and a JSON body.
func (t adminTool) request(args map[string]any) (string, []byte, error) {
	path := t.Path
	consumed := map[string]bool{}
	for {
		i := strings.IndexByte(path, '{')
		if i < 0 {
			break
		}
		j := strings.IndexByte(path[i:], '}')
		if j < 0 {
			return "", nil, fmt.Errorf("malformed path template %q", t.Path)
		}
		name := path[i+1 : i+j]
		v, ok := args[name].(string)
		if !ok || v == "" {
			return "", nil, fmt.Errorf("%s is required", name)
		}
		consumed[name] = true
		path = path[:i] + url.PathEscape(v) + path[i+j+1:]
	}

	if len(t.Body) == 0 {
		return path, nil, nil
	}
	if len(t.Body) == 1 && strings.HasPrefix(t.Body[0], "=") {
		name := t.Body[0][1:]
		v, ok := args[name]
		if !ok {
			return "", nil, fmt.Errorf("%s is required", name)
		}
		b, err := json.Marshal(v)
		return path, b, err
	}
	payload := map[string]any{}
	if len(t.Body) == 1 && t.Body[0] == "*" {
		// Everything not already spent on the path. An empty object is legitimate here — discovery
		// with no arguments is the first call of a setup.
		for k, v := range args {
			if !consumed[k] {
				payload[k] = v
			}
		}
	} else {
		for _, k := range t.Body {
			if v, ok := args[k]; ok {
				payload[k] = v
			}
		}
		// A named-field body with nothing in it is a call that would silently do nothing.
		if len(payload) == 0 {
			return "", nil, fmt.Errorf("%s needs at least one of: %s",
				t.Name, strings.Join(t.Body, ", "))
		}
	}
	b, err := json.Marshal(payload)
	return path, b, err
}
