// Package testtable guards the table name a test is about to write to.
//
// It exists because of a real incident: the property test dropped and recreated `otel.otel_traces`,
// and when it was pointed at a production ClickHouse it destroyed 2504 spans of real telemetry.
// The data was recovered with UNDROP TABLE, which is luck rather than a design.
//
// A test that creates and drops tables must not be able to name a table that could be real, so the
// name is checked rather than trusted. The rule is deliberately crude: a test table ends in
// `_test`, and nothing else may be dropped.
package testtable

import (
	"fmt"
	"strings"
)

const suffix = "_test"

// Guard returns an error unless name is unmistakably a test table.
func Guard(name string) error {
	if !strings.HasSuffix(name, suffix) {
		return fmt.Errorf(
			"refusing to use %q as a test table: a test creates and DROPs its table, so the name "+
				"must end in %q. Naming a table that could hold real data is how a test suite "+
				"deletes production", name, suffix)
	}
	return nil
}

// MustGuard is Guard for a test's setup path.
func MustGuard(t interface{ Fatal(...any) }, name string) {
	if err := Guard(name); err != nil {
		t.Fatal(err)
	}
}
