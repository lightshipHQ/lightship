package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/lightshipHQ/lightship/internal/model"
	"github.com/lightshipHQ/lightship/internal/schema"
	"github.com/lightshipHQ/lightship/internal/store"
)

func TestMCPBindingSchemaMatchesRESTContract(t *testing.T) {
	var advertised map[string]any
	for _, tool := range adminTools() {
		if tool.Name == "lightship_set_binding" {
			advertised = tool.Schema
		}
	}
	if advertised == nil {
		t.Fatal("binding tool not advertised")
	}
	properties := advertised["properties"].(map[string]any)
	required := map[string]bool{}
	for _, name := range advertised["required"].([]string) {
		required[name] = true
	}
	binding := schema.Binding{
		Table: "otel.traces", TraceID: "TraceId", Timestamp: "Timestamp",
		SpanID: "SpanId", ParentSpanID: "ParentSpanId", Name: "SpanName",
	}
	typ := reflect.TypeOf(binding)
	if len(properties) != typ.NumField() || len(required) != typ.NumField() {
		t.Fatalf("tool fields differ from Binding: properties=%v required=%v", properties, required)
	}
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		property, ok := properties[name].(map[string]any)
		if !ok || property["type"] != "string" || !required[name] {
			t.Errorf("%s must be advertised as a required string", name)
		}
		missing := binding
		reflect.ValueOf(&missing).Elem().Field(i).SetString("")
		if err := missing.Validate(); err == nil {
			t.Errorf("REST validation no longer requires %s; update the MCP contract", name)
		}
	}
	if advertised["additionalProperties"] != false {
		t.Error("binding schema must reject extra properties just as REST does")
	}
	body, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("PUT", "/schema/binding", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	var decoded schema.Binding
	if !decode(w, r, &decoded) {
		t.Fatalf("advertised request rejected by REST decoding: %s", w.Body.String())
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("advertised request rejected by REST validation: %v", err)
	}
}

func TestFilterSchemaUsesTheSameConfigurationForBoundFields(t *testing.T) {
	compiled, err := model.Compile(schema.Model{
		Binding: schema.Binding{
			Table: "otel.traces", TraceID: "TraceId", Timestamp: "Timestamp",
			SpanID: "SpanId", ParentSpanID: "ParentSpanId", Name: "SpanName",
		},
		Fields: []schema.Field{
			{Name: "TraceId", LogicalType: schema.TypeString, Filterable: true},
			{Name: "SpanName", LogicalType: schema.TypeString, Filterable: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/filter/schema", nil)
	req = req.WithContext(context.WithValue(req.Context(), sessionKey,
		&store.Session{Username: "reader"}))
	body, err := json.Marshal((&Server{}).filterSchemaBody(req, compiled))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Fields []struct {
			Name      string   `json:"name"`
			Type      string   `json:"type"`
			Operators []string `json:"operators"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Fields) != 2 || got.Fields[0].Name != "TraceId" || got.Fields[1].Name != "SpanName" ||
		got.Fields[0].Type != "string" || !contains(got.Fields[0].Operators, "eq") ||
		!contains(got.Fields[1].Operators, "eq") {
		t.Fatalf("configured bound fields were not advertised consistently: %#v", got.Fields)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
