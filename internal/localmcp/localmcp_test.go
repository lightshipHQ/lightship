package localmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalMCPAddsExportAndWritesRowsOutsideContext(t *testing.T) {
	var mu sync.Mutex
	var queries []map[string]any
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/mcp":
			var req request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			w.Header().Set("Content-Type", "application/json")
			if req.Method == "tools/list" {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"lightship_search_traces"}]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		case "/traces/query":
			var query map[string]any
			if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			queries = append(queries, query)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if query["cursor"] == nil {
				_, _ = w.Write([]byte(`{"binding":{"trace_id":"TraceId"},"spans":[{"TraceId":"t1","Value":"first"},{"TraceId":"t1","Value":"second"}],"next_cursor":"page-2"}`))
				return
			}
			_, _ = w.Write([]byte(`{"binding":{"trace_id":"TraceId"},"spans":[{"TraceId":"t2","Value":"third"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()

	exports := filepath.Join(t.TempDir(), "exports")
	now := time.Date(2026, 9, 7, 8, 30, 0, 0, time.UTC)
	server, err := New(Config{BaseURL: remote.URL, APIKey: "secret-key", ExportDir: exports,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"lightship_export_traces","arguments":{"filter":{"conditions":[{"name":"IsError","op":"eq","value":true}]},"columns":["TraceId","Value"]}}}`,
	}, "\n")
	var output bytes.Buffer
	if err := server.Run(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	dec := json.NewDecoder(&output)
	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := dec.Decode(&listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Result.Tools {
		found = found || tool.Name == exportToolName
	}
	if !found {
		t.Fatalf("export tool not added: %s", output.String())
	}
	var exported struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := dec.Decode(&exported); err != nil {
		t.Fatal(err)
	}
	if exported.Result.IsError || len(exported.Result.Content) != 1 {
		t.Fatalf("export failed: %s", output.String())
	}
	var summary exportSummary
	if err := json.Unmarshal([]byte(exported.Result.Content[0].Text), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Pages != 2 || summary.Spans != 3 || !summary.Complete {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if strings.Contains(output.String(), `"Value":"first"`) || strings.Contains(output.String(), "secret-key") {
		t.Fatalf("protocol output contains trace data or credential: %s", output.String())
	}
	rows, err := os.ReadFile(summary.Path)
	if err != nil {
		t.Fatal(err)
	}
	wantRows := "{\"TraceId\":\"t1\",\"Value\":\"first\"}\n" +
		"{\"TraceId\":\"t1\",\"Value\":\"second\"}\n" +
		"{\"TraceId\":\"t2\",\"Value\":\"third\"}\n"
	if string(rows) != wantRows {
		t.Fatalf("rows = %q, want %q", rows, wantRows)
	}
	if info, err := os.Stat(summary.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("trace file mode: info=%v err=%v", info, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 {
		t.Fatalf("got %d query pages", len(queries))
	}
	for i, query := range queries {
		if query["from"] != "2026-09-06T08:30:00Z" || query["to"] != "2026-09-07T08:30:00Z" {
			t.Errorf("page %d window drifted: %#v", i+1, query)
		}
		if query["limit"] != float64(pageLimit) {
			t.Errorf("page %d limit = %v", i+1, query["limit"])
		}
		if _, ok := query["filter"].(map[string]any); !ok {
			t.Errorf("page %d dropped the filter: %#v", i+1, query)
		}
		if columns, ok := query["columns"].([]any); !ok || len(columns) != 2 {
			t.Errorf("page %d dropped the projection: %#v", i+1, query)
		}
	}
	if queries[1]["cursor"] != "page-2" {
		t.Fatalf("second cursor = %v", queries[1]["cursor"])
	}
}

func TestFailedExportLeavesNoPartialArtifact(t *testing.T) {
	page := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		w.Header().Set("Content-Type", "application/json")
		if page == 1 {
			_, _ = w.Write([]byte(`{"binding":{},"spans":[{"TraceId":"t1"}],"next_cursor":"again"}`))
			return
		}
		_, _ = w.Write([]byte(`{"binding":{},"spans":[{"TraceId":"broken"}`))
	}))
	defer remote.Close()

	exports := filepath.Join(t.TempDir(), "exports")
	server, err := New(Config{BaseURL: remote.URL, APIKey: "key", ExportDir: exports})
	if err != nil {
		t.Fatal(err)
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lightship_export_traces","arguments":{}}}`
	var output bytes.Buffer
	if err := server.Run(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"isError":true`) {
		t.Fatalf("failure not reported: %s", output.String())
	}
	entries, err := os.ReadDir(exports)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial export was left behind: %v", entries)
	}
}

func TestLocalMCPPreservesBatchOrderAndNotifications(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := map[string]any{}
		if req.Method == "tools/list" {
			result["tools"] = []any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID, "result": result,
		})
	}))
	defer remote.Close()
	server, err := New(Config{BaseURL: remote.URL, APIKey: "key", ExportDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	input := `[{"jsonrpc":"2.0","id":1,"method":"ping"},` +
		`{"jsonrpc":"2.0","method":"notifications/initialized"},` +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`
	var output bytes.Buffer
	if err := server.Run(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	var responses []struct {
		ID     int `json:"id"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output.Bytes(), &responses); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 2 || responses[0].ID != 1 || responses[1].ID != 2 {
		t.Fatalf("batch responses = %#v", responses)
	}
	if len(responses[1].Result.Tools) != 1 || responses[1].Result.Tools[0].Name != exportToolName {
		t.Fatalf("export tool missing from batched tools/list: %#v", responses[1])
	}
}

func TestNewRejectsUnsafeRemoteConfiguration(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{BaseURL: "file:///tmp/lightship", APIKey: "key"},
		{BaseURL: "https://example.com/path", APIKey: "key"},
		{BaseURL: "https://example.com"},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New(%+v) succeeded", cfg)
		}
	}
}
