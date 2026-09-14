package httpapi

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lightshipHQ/lightship/internal/filter"
	"github.com/lightshipHQ/lightship/internal/model"
	"github.com/lightshipHQ/lightship/internal/schema"
)

// The access model's write surface.
//
// Every mutation here follows the same three steps: read the current model, apply the change to a
// copy, and compile it. Only a model that compiles is stored. That is why an invalid policy is a
// 400 on the request that proposed it and the running system is untouched — where a config file
// meant the same typo stopped the next boot, turning a mistake into an outage.
//
// After a successful write the replica refreshes its own snapshot, so its next request sees the
// change without waiting out the TTL. Other replicas converge within it.

// apply validates a proposed model; persistence succeeds only while its read version is current.
func (s *Server) apply(w http.ResponseWriter, r *http.Request, next schema.Model,
	persist func() error) bool {
	if err := model.Validate(next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return false
	}
	if err := persist(); err != nil {
		storeErr(w, err)
		return false
	}
	if _, err := s.models.Refresh(r.Context()); err != nil {
		// The write committed, but the local snapshot is invalidated. Queries fail closed until
		// a reload succeeds; the successful mutation must not be reported as a failed write.
		s.log.Error("model stored but refresh failed", "err", err)
	}
	return true
}

// current is the compiled model, for the paths that genuinely need one: serving a query, or
// describing the filter vocabulary.
func (s *Server) current(w http.ResponseWriter, r *http.Request) (*model.Compiled, bool) {
	m, err := s.models.Get(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return nil, false
	}
	return m, true
}

// stored is the raw model, straight from Postgres and never compiled.
//
// Every write reads through here rather than through the compiled snapshot, because otherwise a
// model that does not compile would brick the API that exists to repair it: the admin could neither
// see what was wrong nor overwrite it. A stored model should never fail to compile — writes
// validate first — but "should never" is not a state to be locked out by.
func (s *Server) stored(w http.ResponseWriter, r *http.Request) (schema.Model, bool) {
	m, err := s.st.LoadModel(r.Context())
	if err != nil {
		storeErr(w, err)
		return schema.Model{}, false
	}
	return m, true
}

// discover reports what is in the customer's ClickHouse. It decides nothing: an operator, or their
// agent, marks what lightship may reference and posts it back.
func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		// Table and Timestamp scope key discovery to one candidate. Omit them and only the table
		// listing comes back, which is the first call of a setup.
		Table     string   `json:"table"`
		Timestamp string   `json:"timestamp"`
		Maps      []string `json:"maps"`
		Window    string   `json:"window"` // Go duration; how far back to sample for keys
		From      string   `json:"from"`
		To        string   `json:"to"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}

	tables, err := s.reader.DiscoverTables(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway,
			map[string]any{"error": "could not read the source schema: " + err.Error()})
		return
	}
	out := map[string]any{"tables": tables}

	if body.Table != "" && len(body.Maps) > 0 {
		if body.Timestamp == "" {
			writeJSON(w, http.StatusBadRequest,
				map[string]any{"error": "timestamp is required to sample keys over a window"})
			return
		}
		window, err := windowFrom(body.From, body.To)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		// Keep the duration input for API and MCP clients written before custom bounds were added.
		if body.From == "" && body.To == "" && body.Window != "" {
			d, err := time.ParseDuration(body.Window)
			if err != nil || d <= 0 {
				writeJSON(w, http.StatusBadRequest,
					map[string]any{"error": `window must be a positive Go duration, e.g. "168h"`})
				return
			}
			window.From = window.To.Add(-d)
		}
		// Key sampling needs only a table and a timestamp, not a full binding — it runs during
		// setup, before one exists. The two names are checked here because they are interpolated
		// into the sampling SQL.
		b := schema.Binding{Table: body.Table, Timestamp: body.Timestamp}
		if !schema.ValidTable(b.Table) {
			writeJSON(w, http.StatusBadRequest,
				map[string]any{"error": fmt.Sprintf(
					"table must be database.table using plain identifiers, got %q", b.Table)})
			return
		}
		if !schema.ValidIdent(b.Timestamp) {
			writeJSON(w, http.StatusBadRequest,
				map[string]any{"error": fmt.Sprintf("%q is not a usable timestamp column name", b.Timestamp)})
			return
		}
		keys, err := s.reader.DiscoverKeys(r.Context(), b, body.Maps, window)
		if err != nil {
			writeJSON(w, http.StatusBadGateway,
				map[string]any{"error": "could not sample attribute keys: " + err.Error()})
			return
		}
		out["keys"] = keys
		out["from"] = window.From
		out["to"] = window.To
		out["window"] = window.To.Sub(window.From).String()
		out["note"] = "coverage below 1.0 means some spans lack the key; a trace is visible when " +
			"at least one span satisfies the policy, even if other spans lack the key"
	}
	s.audit(r, "schema.discover", "", map[string]any{"tables": len(tables)})
	writeJSON(w, http.StatusOK, out)
}

// getSchema returns the whole stored model. Admin-only, and the thing to commit to git if you want
// the access model reviewable — it is an export, not a source of truth, so nothing reconciles it.
func (s *Server) getSchema(w http.ResponseWriter, r *http.Request) {
	m, ok := s.stored(w, r)
	if !ok {
		return
	}
	// Report whether it compiles rather than refusing to show it. An admin looking at a broken
	// model needs to see the model.
	out := map[string]any{"model": m, "compiles": true}
	if err := model.Validate(m); err != nil {
		out["compiles"] = false
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putBinding(w http.ResponseWriter, r *http.Request) {
	var b schema.Binding
	if !decode(w, r, &b) {
		return
	}
	if err := b.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !s.checkColumns(w, r, b.Table, map[string]string{
		b.TraceID: "trace_id", b.Timestamp: "timestamp", b.SpanID: "span_id",
		b.ParentSpanID: "parent_span_id", b.Name: "name",
	}) {
		return
	}
	next, ok := s.stored(w, r)
	if !ok {
		return
	}
	next.Binding = b
	if !s.apply(w, r, next, func() error { return s.st.SetBinding(r.Context(), b, next.Version) }) {
		return
	}
	s.audit(r, "schema.binding.set", "", map[string]any{"table": b.Table,
		"trace_id": b.TraceID, "timestamp": b.Timestamp})
	writeJSON(w, http.StatusOK, b)
}

// putFields replaces the whole marked set. Marking is one reviewable statement of what lightship
// may reference — a field that drops out of that statement should stop being referenceable, rather
// than linger because nobody sent a delete for it.
func (s *Server) putFields(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Fields []schema.Field `json:"fields"`
	}
	if !decode(w, r, &body) {
		return
	}
	for i := range body.Fields {
		f := &body.Fields[i]
		f.LogicalType = f.Type()
		if err := f.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest,
				map[string]any{"error": "field " + f.Ref() + ": " + err.Error()})
			return
		}
	}
	next, ok := s.stored(w, r)
	if !ok {
		return
	}
	if next.Binding.Table != "" {
		needed := map[string]string{}
		for _, f := range body.Fields {
			if f.Map != "" {
				needed[f.Map] = "map column for " + f.Ref()
				continue
			}
			needed[f.Name] = "marked column"
		}
		if !s.checkColumns(w, r, next.Binding.Table, needed) {
			return
		}
	}
	next.Fields = body.Fields
	// Compiling here is what catches the dangerous case: unmarking a field some policy still
	// references. The policy would otherwise stop constraining and nobody would be told.
	if !s.apply(w, r, next, func() error {
		return s.st.PutFields(r.Context(), body.Fields, next.Version)
	}) {
		return
	}
	s.audit(r, "schema.fields.set", "", map[string]any{"fields": len(body.Fields)})
	writeJSON(w, http.StatusOK, map[string]any{"fields": body.Fields,
		"user_attributes": next.UserAttrs})
}

func (s *Server) putRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Policies []schema.Policy `json:"policies"`
	}
	if !decode(w, r, &body) {
		return
	}
	role := schema.Role{Name: r.PathValue("name"), Policies: body.Policies}
	if role.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "role name is required"})
		return
	}
	if len(role.Policies) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "a role must carry at least one policy: one with none grants nothing, and is " +
				"more likely a mistake than an intent"})
		return
	}
	cur, ok := s.stored(w, r)
	if !ok {
		return
	}
	next := cur
	next.Roles = append([]schema.Role{}, cur.Roles...)
	replaced := false
	for i := range next.Roles {
		if next.Roles[i].Name == role.Name {
			next.Roles[i] = role
			replaced = true
		}
	}
	if !replaced {
		next.Roles = append(next.Roles, role)
	}
	if !s.apply(w, r, next, func() error { return s.st.PutRole(r.Context(), role, next.Version) }) {
		return
	}
	s.audit(r, "role.set", "", map[string]any{"role": role.Name, "policies": role.Policies})
	writeJSON(w, http.StatusOK, role)
}

// deleteRole removes the role and every assignment of it. Callers holding it lose it on their next
// request, because roles are re-read per request — this is a revocation, not a request to expire.
func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cur, ok := s.stored(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteRole(r.Context(), name, cur.Version); err != nil {
		storeErr(w, err)
		return
	}
	if _, err := s.models.Refresh(r.Context()); err != nil {
		s.log.Error("role deleted but refresh failed", "err", err)
	}
	s.audit(r, "role.delete", "", map[string]any{"role": name})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

// filterSchema is what an agent reads to build a query it knows will be accepted. Because the
// marked set is declared rather than inferred, this is complete: there is no field a filter could
// name that is not listed here.
func (s *Server) filterSchema(w http.ResponseWriter, r *http.Request) {
	m, ok := s.current(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.filterSchemaBody(r, m))
}

func (s *Server) filterSchemaBody(r *http.Request, m *model.Compiled) map[string]any {
	type field struct {
		Map  string `json:"map,omitempty"`
		Name string `json:"name"`
		Ref  string `json:"ref"`
		Type string `json:"type"`
		// Operators is absent on a field that cannot be filtered. Listing them for a policy-only
		// field would advertise a query that will be refused.
		Operators  []string `json:"operators,omitempty"`
		Filterable *bool    `json:"filterable,omitempty"`
		Policy     *bool    `json:"policy,omitempty"`
	}
	admin := sessionFrom(r.Context()).IsAdmin()

	out := []field{}
	for _, f := range m.Schema.Fields {
		if !f.Filterable && !admin {
			continue
		}
		e := field{Map: f.Map, Name: f.Name, Ref: f.Ref(), Type: string(f.Type())}
		if f.Filterable {
			e.Operators = filter.OperatorsFor(f.Type())
		}
		if admin {
			// Which fields carry the security model is an admin's business. A caller only needs
			// to know what they may narrow by, so they never see a field they cannot filter.
			p, fl := f.Policy, f.Filterable
			e.Policy, e.Filterable = &p, &fl
		}
		out = append(out, e)
	}
	return map[string]any{
		"binding": m.Schema.Binding,
		"fields":  out,
		"version": m.Schema.Version,
		"limits": map[string]any{
			"max_conditions": filter.MaxConditions,
			"max_values":     filter.MaxValues,
			"max_limit":      maxLimit,
		},
		// Said plainly because an agent cannot infer it and will otherwise write the wrong filter:
		// conditions AND together, and a condition matches a trace when any of its spans matches.
		"semantics": "conditions are ANDed; a trace matches when any of its spans satisfies them",
	}
}

// checkColumns verifies that names the model uses actually exist in the table it is bound to.
//
// Without it a binding can be pointed at a different table while the marked fields still name the
// old one's columns, and the mistake surfaces as a raw ClickHouse error on somebody's next query.
// The check costs one read of system.columns on a write that happens rarely.
func (s *Server) checkColumns(w http.ResponseWriter, r *http.Request, table string,
	needed map[string]string) bool {
	info, err := s.reader.Describe(r.Context(), table)
	if err != nil {
		writeJSON(w, http.StatusBadGateway,
			map[string]any{"error": "could not read the table's columns: " + err.Error()})
		return false
	}
	var missing []string
	for col, why := range needed {
		if col != "" && !info.Has(col) {
			missing = append(missing, fmt.Sprintf("%s (%s)", col, why))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		have := make([]string, 0, len(info.Columns))
		for _, c := range info.Columns {
			have = append(have, c.Name)
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "table " + table + " has no column: " + strings.Join(missing, ", "),
			"columns": have,
		})
		return false
	}
	return true
}
