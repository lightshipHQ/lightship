package policy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lightshipHQ/lightship/internal/expression"
	"github.com/lightshipHQ/lightship/internal/schema"
)

type Query = expression.Query

func (e *Env) Translate(p *Program, attrs map[string]string) (Query, error) {
	q, err := expression.Compile(p.root, expression.PolicyProfile, attrs)
	if errors.Is(err, expression.ErrUnsatisfiable) {
		return Query{}, fmt.Errorf("%w: %v", ErrUnsatisfiable, err)
	}
	return q, err
}

func (e *Env) Predicate(programs []*Program, attrs map[string]string) (Query, error) {
	var clauses []string
	var args []any
	for _, p := range programs {
		q, err := e.Translate(p, attrs)
		if err != nil {
			continue
		}
		clauses = append(clauses, q.SQL)
		args = append(args, q.Args...)
	}
	if len(clauses) == 0 {
		return Query{}, ErrUnsatisfiable
	}
	return Query{SQL: "(" + strings.Join(clauses, " OR ") + ")", Args: args}, nil
}

func Having(q Query) Query { return Query{SQL: fmt.Sprintf("countIf(%s) > 0", q.SQL), Args: q.Args} }

func TraceIDs(b schema.Binding, scope Query, q Query) Query {
	where := q.SQL
	args := append([]any{}, scope.Args...)
	if scope.SQL != "" {
		where = scope.SQL + " AND " + q.SQL
	}
	args = append(args, q.Args...)
	return Query{SQL: fmt.Sprintf("SELECT DISTINCT %s FROM %s WHERE %s", b.TraceID, b.Table, where), Args: args}
}
