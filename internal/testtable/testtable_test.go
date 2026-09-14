package testtable

import "testing"

func TestRejectsRealTable(t *testing.T) {
	if Guard("otel.otel_traces") == nil {
		t.Fatal("must refuse a table that could be real")
	}
	if Guard("otel.lightship_property_test") != nil {
		t.Fatal("must allow a test table")
	}
}
