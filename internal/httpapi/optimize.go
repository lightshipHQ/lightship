package httpapi

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/lightshipHQ/lightship/internal/schema"
)

// Optimisation suggestions.
//
// LightShip knows something no general-purpose tool does: the closed set of fields a policy or a
// filter may name. That is exactly the input for deciding which skip indexes would pay for
// themselves, so the suggestion is derived rather than guessed.
//
// It is returned as DDL for an operator to apply, never executed. The ClickHouse credentials are
// read-only on purpose, and a product whose whole claim is about restraint on someone else's data
// should not be issuing ALTER TABLE against it.
//
// The basis is the marked set, not the policies currently written against it. Marking is where an
// operator deliberately states which fields matter, and it changes rarely; policies change often,
// and an index is far too expensive to create and backfill on that cadence.
//
// One caveat worth knowing rather than acting on: today only the policy reaches the trace-selection
// subquery's WHERE, which is the only place a skip index acts. A caller's filter is a HAVING over
// grouped rows, so an index on a filter-only field will not be used yet. It is still worth having —
// it costs storage, not correctness.
//
// Measured on a 2M-span day with a tenant-scoped policy: with a bloom filter over the map's values,
// the trace-selection subquery read 352k rows in 11ms; without one, 2.00M rows in 16ms. An index on
// the exact key expression added nothing beyond the generic one, which is why the suggestion is one
// index per map column rather than one per field.

type suggestion struct {
	Target string `json:"target"`
	Status string `json:"status"` // present | missing
	Reason string `json:"reason"`
	DDL    string `json:"ddl,omitempty"`
}

func (s *Server) optimizations(w http.ResponseWriter, r *http.Request) {
	m, ok := s.current(w, r)
	if !ok {
		return
	}
	if err := m.Ready(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	table := m.Schema.Binding.Table

	info, err := s.reader.Describe(r.Context(), table)
	if err != nil {
		writeJSON(w, http.StatusBadGateway,
			map[string]any{"error": "could not read the table's indexes: " + err.Error()})
		return
	}

	// An index counts as covering a target when its expression mentions it. Deliberately loose: it
	// recognises `mapValues(ResourceAttributes)` and also ClickStack's `ResourceAttributeItems`
	// text index, and the cost of a false positive is a suggestion nobody needed rather than a
	// wrong query.
	covers := func(needle string) (string, bool) {
		for _, idx := range info.Indexes {
			if strings.Contains(idx.Expr, needle) {
				return idx.Name, true
			}
		}
		return "", false
	}

	out := []suggestion{}
	maps := map[string][]string{}
	var scalars []string
	// Everything marked, whether for policy or for filtering. The marking step is where an operator
	// deliberately says which fields matter; policies churn, and an index is far too expensive to
	// create and materialise on that cadence.
	for _, f := range m.Schema.Fields {
		if !f.Policy && !f.Filterable {
			continue
		}
		if f.Map != "" {
			maps[f.Map] = append(maps[f.Map], f.Name)
			continue
		}
		scalars = append(scalars, f.Name)
	}
	mapNames := make([]string, 0, len(maps))
	for k := range maps {
		mapNames = append(mapNames, k)
	}
	sort.Strings(mapNames)

	for _, col := range mapNames {
		keys := maps[col]
		sort.Strings(keys)
		if name, ok := covers(col); ok {
			out = append(out, suggestion{
				Target: col, Status: "present",
				Reason: fmt.Sprintf("index %q already covers this map; it serves all %d marked key(s)",
					name, len(keys)),
			})
			continue
		}
		idxVal := indexName(col + "_values")
		idxKey := indexName(col + "_keys")
		out = append(out, suggestion{
			Target: col, Status: "missing",
			Reason: fmt.Sprintf("%d marked key(s) live in this map (%s) and no skip index covers it",
				len(keys), strings.Join(keys, ", ")),
			DDL: fmt.Sprintf(
				"ALTER TABLE %s\n  ADD INDEX %s mapValues(%s) TYPE bloom_filter(0.01) GRANULARITY 1,\n"+
					"  ADD INDEX %s mapKeys(%s) TYPE bloom_filter(0.01) GRANULARITY 1;\n"+
					"-- ADD INDEX applies to new parts only; this backfills the existing ones:\n"+
					"ALTER TABLE %s MATERIALIZE INDEX %s;\nALTER TABLE %s MATERIALIZE INDEX %s;",
				table, idxVal, col, idxKey, col, table, idxVal, table, idxKey),
		})
	}

	sort.Strings(scalars)
	for _, col := range scalars {
		if strings.Contains(info.SortingKey, col) {
			out = append(out, suggestion{Target: col, Status: "present",
				Reason: "in the table's sorting key, so a comparison on it is already a range scan"})
			continue
		}
		if name, ok := covers(col); ok {
			out = append(out, suggestion{Target: col, Status: "present",
				Reason: fmt.Sprintf("index %q covers this column", name)})
			continue
		}
		idx := indexName(col)
		out = append(out, suggestion{Target: col, Status: "missing",
			Reason: "marked, but neither in the sorting key nor covered by a skip index",
			DDL: fmt.Sprintf(
				"ALTER TABLE %s ADD INDEX %s %s TYPE bloom_filter(0.01) GRANULARITY 1;\n"+
					"ALTER TABLE %s MATERIALIZE INDEX %s;", table, idx, col, table, idx),
		})
	}

	var all []string
	for _, sg := range out {
		if sg.DDL != "" {
			all = append(all, sg.DDL)
		}
	}
	s.audit(r, "schema.optimizations", "", map[string]any{"missing": len(all)})
	writeJSON(w, http.StatusOK, map[string]any{
		"table":       table,
		"sorting_key": info.SortingKey,
		"indexes":     info.Indexes,
		"suggestions": out,
		// One block to paste. Empty when there is nothing to do.
		"ddl": strings.Join(all, "\n\n"),
		"note": "lightship holds read-only ClickHouse credentials and will not run this. Apply it " +
			"yourself; MATERIALIZE INDEX rewrites existing parts and can be slow on a large table. " +
			"An index on a field only a filter names will not be used yet — filters are evaluated " +
			"after grouping — but costs only storage.",
	})
}

// indexName keeps a generated identifier recognisable and valid.
func indexName(s string) string {
	b := strings.Builder{}
	b.WriteString("idx_ls_")
	for _, r := range strings.ToLower(s) {
		if schema.ValidIdent(string(r)) || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
