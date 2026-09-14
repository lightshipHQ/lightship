package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lightshipHQ/lightship/internal/filter"
	"github.com/lightshipHQ/lightship/internal/policy"
	"github.com/lightshipHQ/lightship/internal/store"
	"github.com/lightshipHQ/lightship/internal/traces"
)

const (
	defaultLimit = 50
	maxLimit     = 200
	defaultSpan  = 24 * time.Hour
)

// query assembles everything one read needs: the operator's discovered binding, the compiled
// vocabulary, and the caller's programs. All of it comes from a single model snapshot, so a request
// is never served by fields from before a change and policies from after.
func (s *Server) query(r *http.Request, sess *store.Session) (traces.Query, error) {
	m, err := s.models.Get(r.Context())
	if err != nil {
		return traces.Query{}, err
	}
	if err := m.Ready(); err != nil {
		return traces.Query{}, err
	}
	var progs []*policy.Program
	for _, role := range sess.Roles {
		if role == store.AdminRole {
			// The admin role's stored policy is `true`, and m.Admin is that same program compiled
			// independently. Taking it from the registry as well would OR `true` with itself in
			// every admin query — harmless, but it is the sort of redundancy that later reads as
			// two enforcement paths where there is one.
			continue
		}
		progs = append(progs, m.Registry[role]...)
	}
	if sess.IsAdmin() && m.Admin != nil {
		progs = append(progs, m.Admin)
	}
	return traces.Query{
		Binding: m.Schema.Binding,
		Env:     m.Env,
		Model:   m.Schema,
		Caller:  traces.Caller{Username: sess.Username, Attrs: sess.Attrs, Programs: progs},
	}, nil
}

// notReady turns a missing or unusable model into a clear 503 rather than a query that cannot be
// built. Fail closed: no binding means no quantifier, and no quantifier means no guarantee.
func (s *Server) notReady(w http.ResponseWriter, err error) {
	s.log.Warn("query refused: access model not ready", "err", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
}

// listResult is the whole read, returning a body and a status rather than writing one. Both the
// HTTP handlers and the MCP tools go through it, so there is one query path and one place where the
// policy and the filter meet — a second would be how a filter gets forgotten.
func (s *Server) listResult(r *http.Request, win traces.Window, f filter.Filter,
	limit int, cursor string) (map[string]any, int) {
	sess := sessionFrom(r.Context())
	cur, curFilter, err := decodeCursor(cursor)
	if err != nil {
		return map[string]any{"error": "malformed cursor"}, http.StatusBadRequest
	}
	fh := filterHash(f)
	if cur != nil && curFilter != fh {
		return map[string]any{
			"error": "this cursor belongs to a different filter: start the scroll again",
		}, http.StatusBadRequest
	}

	q, err := s.query(r, sess)
	if err != nil {
		s.log.Warn("query refused: access model not ready", "err", err)
		return map[string]any{"error": err.Error()}, http.StatusServiceUnavailable
	}
	var pred *policy.Query
	if !f.Empty() {
		built, err := f.Build(q.Model)
		if err != nil {
			return map[string]any{"error": err.Error()}, http.StatusBadRequest
		}
		pred = &built
	}

	res, err := s.reader.List(r.Context(), q, win, pred, cur, limit)
	if err != nil {
		s.log.Error("list traces", "err", err)
		return map[string]any{"error": "trace store unavailable"}, http.StatusBadGateway
	}

	// The filter and the result count, never the ids: one broad listing would otherwise write
	// thousands of audit rows.
	detail := map[string]any{
		"from": win.From, "to": win.To, "returned": len(res.Traces),
	}
	if !f.Empty() {
		detail["filter"] = f.Conditions
	}
	s.audit(r, "query.list", strings.Join(sess.Roles, ","), detail)

	body := map[string]any{"traces": res.Traces}
	// A next cursor only when the page was full: a short page is the end of the scroll.
	if res.Next != nil && len(res.Traces) == limit {
		body["next_cursor"] = encodeCursor(res.Next, fh)
	}
	return body, http.StatusOK
}

func (s *Server) traceDetail(w http.ResponseWriter, r *http.Request) {
	body, code := s.detailResult(r, r.PathValue("id"), projection(r.URL.Query().Get("columns")))
	writeJSON(w, code, body)
}

// projection parses `columns=a,b,c` from a query string. Empty means every column of the row, which
// is the right default for a client that has not said otherwise and the wrong one for a large table.
func projection(v string) traces.Projection {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	out := traces.Projection{}
	for _, c := range strings.Split(v, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// queryTraces is the read. It returns rows — the customer's own columns, as stored — for every
// trace on one page of the scroll, which is what a client streams to a file and analyses there.
//
// It is a POST because a filter does not belong in a URL: generated filters are long, and the
// filter is audited anyway, so nothing is gained by having it in a request line.
//
// limit counts traces, not spans. The policy is a trace-level predicate and the cursor is a trace
// position, so a page is N traces and however many spans they hold.
//
// Summaries are not a REST shape. An aggregate per trace is cheap enough to hand a model, so it
// lives where reasoning happens — the lightship_search_traces MCP tool — while rows live here.
func (s *Server) queryTraces(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filter  filter.Filter `json:"filter"`
		From    string        `json:"from"`
		To      string        `json:"to"`
		Limit   int           `json:"limit"`
		Cursor  string        `json:"cursor"`
		Columns []string      `json:"columns"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}
	win, err := windowFrom(body.From, body.To)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.checkWindow(win); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	limit := body.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	s.streamSpans(w, r, win, body.Filter, limit, body.Cursor, body.Columns)
}

type spanPlan struct {
	query      traces.Query
	predicate  *policy.Query
	cursor     *traces.Cursor
	filterHash string
}

func (s *Server) planSpans(r *http.Request, f filter.Filter, cursor string,
	p traces.Projection) (*spanPlan, map[string]any, int) {
	if err := p.Validate(); err != nil {
		return nil, map[string]any{"error": err.Error()}, http.StatusBadRequest
	}
	sess := sessionFrom(r.Context())
	cur, curFilter, err := decodeCursor(cursor)
	if err != nil {
		return nil, map[string]any{"error": "malformed cursor"}, http.StatusBadRequest
	}
	fh := filterHash(f)
	if cur != nil && curFilter != fh {
		return nil, map[string]any{
			"error": "this cursor belongs to a different filter: start the scroll again",
		}, http.StatusBadRequest
	}
	q, err := s.query(r, sess)
	if err != nil {
		s.log.Warn("query refused: access model not ready", "err", err)
		return nil, map[string]any{"error": err.Error()}, http.StatusServiceUnavailable
	}
	var pred *policy.Query
	if !f.Empty() {
		built, err := f.Build(q.Model)
		if err != nil {
			return nil, map[string]any{"error": err.Error()}, http.StatusBadRequest
		}
		pred = &built
	}
	return &spanPlan{query: q, predicate: pred, cursor: cur, filterHash: fh}, nil,
		http.StatusOK
}

func (s *Server) streamSpans(w http.ResponseWriter, r *http.Request, win traces.Window,
	f filter.Filter, limit int, cursor string, p traces.Projection) {
	plan, body, code := s.planSpans(r, f, cursor, p)
	if body != nil {
		writeJSON(w, code, body)
		return
	}

	rows, err := s.reader.Spans(r.Context(), plan.query, win, plan.predicate, plan.cursor, limit, p)
	if err != nil {
		s.log.Error("list spans", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "trace store unavailable"})
		return
	}
	defer rows.Close()

	count, started, err := writeSpanPage(w, plan.query.Binding, rows, plan.filterHash)
	if err != nil {
		s.log.Error("stream spans", "err", err)
		if !started {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "trace store unavailable"})
		}
		return
	}

	sess := sessionFrom(r.Context())
	detail := map[string]any{
		"from": win.From, "to": win.To, "returned_spans": count, "traces": limit,
	}
	if !f.Empty() {
		detail["filter"] = f.Conditions
	}
	if len(p) > 0 {
		detail["columns"] = []string(p)
	}
	s.audit(r, "query.traces", strings.Join(sess.Roles, ","), detail)
}

// writeSpanPage commits the response only after the first ClickHouse row has been decoded. Once it
// has started, a later source or connection error deliberately leaves an incomplete JSON document;
// a downloader must never mistake a partial page for a successful one.
type spanReader interface {
	Read() (traces.Span, bool, error)
	NextCursor() *traces.Cursor
}

func writeSpanPage(w http.ResponseWriter, binding any, rows spanReader,
	filterHash string) (count int, started bool, err error) {
	first, ok, err := rows.Read()
	if err != nil {
		return 0, false, err
	}
	bindingJSON, err := json.Marshal(binding)
	if err != nil {
		return 0, false, err
	}
	var firstJSON []byte
	if ok {
		firstJSON, err = json.Marshal(first)
		if err != nil {
			return 0, false, err
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	started = true
	if _, err = w.Write(append([]byte(`{"binding":`), bindingJSON...)); err != nil {
		return 0, true, err
	}
	if _, err = w.Write([]byte(`,"spans":[`)); err != nil {
		return 0, true, err
	}
	if ok {
		if _, err = w.Write(firstJSON); err != nil {
			return 0, true, err
		}
		count = 1
	}
	for {
		span, ok, readErr := rows.Read()
		if readErr != nil {
			return count, true, readErr
		}
		if !ok {
			break
		}
		encoded, marshalErr := json.Marshal(span)
		if marshalErr != nil {
			return count, true, marshalErr
		}
		if count > 0 {
			if _, err = w.Write([]byte{','}); err != nil {
				return count, true, err
			}
		}
		if _, err = w.Write(encoded); err != nil {
			return count, true, err
		}
		count++
	}
	if _, err = w.Write([]byte{']'}); err != nil {
		return count, true, err
	}
	if rows.NextCursor() != nil {
		cursorJSON, marshalErr := json.Marshal(encodeCursor(rows.NextCursor(), filterHash))
		if marshalErr != nil {
			return count, true, marshalErr
		}
		if _, err = w.Write(append([]byte(`,"next_cursor":`), cursorJSON...)); err != nil {
			return count, true, err
		}
	}
	_, err = w.Write([]byte("}\n"))
	return count, true, err
}

// detailResult returns every span of one trace. Denied and absent are the same response: otherwise
// the status distinguishes them and becomes an oracle for which trace ids exist.
func (s *Server) detailResult(r *http.Request, id string, p traces.Projection) (map[string]any, int) {
	if err := p.Validate(); err != nil {
		return map[string]any{"error": err.Error()}, http.StatusBadRequest
	}
	sess := sessionFrom(r.Context())
	q, err := s.query(r, sess)
	if err != nil {
		s.log.Warn("query refused: access model not ready", "err", err)
		return map[string]any{"error": err.Error()}, http.StatusServiceUnavailable
	}
	spans, err := s.reader.Detail(r.Context(), q, id, p)
	if errors.Is(err, traces.ErrNotVisible) {
		s.audit(r, "query.detail", strings.Join(sess.Roles, ","),
			map[string]any{"trace_id": id, "visible": false})
		return map[string]any{"error": "trace not found"}, http.StatusNotFound
	}
	if err != nil {
		s.log.Error("trace detail", "err", err)
		return map[string]any{"error": "trace store unavailable"}, http.StatusBadGateway
	}
	s.audit(r, "query.detail", strings.Join(sess.Roles, ","),
		map[string]any{"trace_id": id, "visible": true, "spans": len(spans)})
	// The binding travels with the response so a client can find the structural columns — which one
	// is the span id, which is the parent — without having to know the customer's table itself.
	return map[string]any{
		"trace_id": id,
		"binding":  q.Binding,
		"spans":    spans,
	}, http.StatusOK
}

func (s *Server) checkWindow(win traces.Window) error {
	if s.limits.MaxQueryWindow > 0 && win.To.Sub(win.From) > s.limits.MaxQueryWindow {
		return fmt.Errorf("query window exceeds maximum of %s", s.limits.MaxQueryWindow)
	}
	return nil
}

// listAudit is admin-only — the log records who looked at what, so handing it to every caller would
// leak the shape of the policies and who is reading which data. The check is the adminOnly wrapper
// on the route, like every other admin route: a guard inside one handler is a guard nobody reading
// the route table can see.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListAudit(r.Context(), intParam(r, "limit", 100, 1000))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not read audit log"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows})
}

// windowFrom parses the time window. Absent bounds default to the last 24 hours, so a client that
// says nothing gets something bounded rather than the whole table.
func windowFrom(fromStr, toStr string) (traces.Window, error) {
	to := time.Now()
	from := to.Add(-defaultSpan)
	if fromStr != "" {
		t, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			return traces.Window{}, errors.New("from must be RFC3339")
		}
		from = t
	}
	if toStr != "" {
		t, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			return traces.Window{}, errors.New("to must be RFC3339")
		}
		to = t
	}
	if !from.Before(to) {
		return traces.Window{}, errors.New("from must be before to")
	}
	return traces.Window{From: from, To: to}, nil
}

func intParam(r *http.Request, name string, def, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// The cursor is opaque to clients but carries no authority: it only positions a scroll, and the
// policy is re-applied on every page regardless of what it says.
type cursorWire struct {
	N int64  `json:"n"`
	I string `json:"i"`
	// F binds the cursor to the filter it was produced under. Page 2 of a different filter is not
	// page 2 of anything — the scroll would silently repeat and skip — so a mismatch is refused
	// rather than served.
	F string `json:"f,omitempty"`
}

func encodeCursor(c *traces.Cursor, filterHash string) string {
	b, _ := json.Marshal(cursorWire{N: c.StartNanos, I: c.TraceID, F: filterHash})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*traces.Cursor, string, error) {
	if s == "" {
		return nil, "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, "", err
	}
	var w cursorWire
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, "", err
	}
	return &traces.Cursor{StartNanos: w.N, TraceID: w.I}, w.F, nil
}

// filterHash is short on purpose: it exists to catch a filter that changed mid-scroll, not to
// authenticate anything. The cursor carries no authority — every page re-resolves the caller.
func filterHash(f filter.Filter) string {
	if f.Empty() {
		return ""
	}
	sum := sha256.Sum256([]byte(f.Canonical()))
	return hex.EncodeToString(sum[:8])
}
