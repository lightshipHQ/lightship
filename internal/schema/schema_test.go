package schema

import "testing"

func TestValidTableRequiresTwoPlainIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"otel.traces", true},
		{"lightship_demo.agent_traces", true},
		{"traces", false},
		{"otel.public.traces", false},
		{"otel.", false},
		{".traces", false},
		{"otel.trace-name", false},
		{"otel.traces;DROP TABLE users", false},
		{"`otel`.`traces`", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidTable(tc.name); got != tc.want {
				t.Fatalf("ValidTable(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestBindingRejectsUnsafeTableBeforeUse(t *testing.T) {
	binding := Binding{
		Table: "otel.traces;DROP TABLE users", TraceID: "TraceId", Timestamp: "Timestamp",
		SpanID: "SpanId", ParentSpanID: "ParentSpanId", Name: "SpanName",
	}
	if err := binding.Validate(); err == nil {
		t.Fatal("unsafe table binding was accepted")
	}
}
