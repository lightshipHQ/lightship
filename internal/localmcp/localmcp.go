// Package localmcp runs the LightShip analysis MCP beside a coding agent. It proxies the bounded
// conversational tools to the remote server and keeps bulk trace rows out of MCP by streaming them
// directly into a workspace file.
package localmcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	exportToolName = "lightship_export_traces"
	pageLimit      = 200
	maxProxyBody   = 2 << 20
)

// Config is the local companion's complete runtime configuration. ExportDir defaults to
// .lightship/exports under the process working directory.
type Config struct {
	BaseURL   string
	APIKey    string
	ExportDir string
	Client    *http.Client
	Now       func() time.Time
}

type Server struct {
	baseURL   string
	apiKey    string
	exportDir string
	client    *http.Client
	now       func() time.Time
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type exportArgs struct {
	From    string          `json:"from"`
	To      string          `json:"to"`
	Filter  json.RawMessage `json:"filter"`
	Columns []string        `json:"columns"`
}

type exportSummary struct {
	ExportID string `json:"export_id"`
	Path     string `json:"path"`
	Metadata string `json:"metadata_path"`
	Pages    int    `json:"pages"`
	Spans    int    `json:"spans"`
	From     string `json:"from"`
	To       string `json:"to"`
	Complete bool   `json:"complete"`
}

// New validates the remote and prepares a local companion. It never reads credentials from MCP
// client configuration files; the launcher supplies the key directly to this process.
func New(cfg Config) (*Server, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("LIGHTSHIP_URL must be an http or https origin")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("LIGHTSHIP_URL must not include a path")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("LIGHTSHIP_API_KEY is required")
	}
	exportDir := cfg.ExportDir
	if exportDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("find workspace: %w", err)
		}
		exportDir = filepath.Join(cwd, ".lightship", "exports")
	}
	abs, err := filepath.Abs(exportDir)
	if err != nil {
		return nil, fmt.Errorf("resolve export directory: %w", err)
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Server{baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKey: cfg.APIKey,
		exportDir: abs, client: client, now: now}, nil
}

// Run serves newline-delimited JSON-RPC over stdio. Diagnostics belong on the caller's stderr;
// this function writes only protocol responses to out.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read MCP request: %w", err)
		}
		response, ok := s.handle(ctx, raw)
		if !ok {
			continue
		}
		if err := enc.Encode(response); err != nil {
			return fmt.Errorf("write MCP response: %w", err)
		}
	}
}

func (s *Server) handle(ctx context.Context, raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return protocolError(nil, -32700, "parse error"), true
		}
		if len(batch) == 0 {
			return protocolError(nil, -32600, "invalid request: empty batch"), true
		}
		responses := make([]json.RawMessage, 0, len(batch))
		for _, item := range batch {
			if response, ok := s.handleOne(ctx, item); ok {
				responses = append(responses, response)
			}
		}
		if len(responses) == 0 {
			return nil, false
		}
		encoded, _ := json.Marshal(responses)
		return encoded, true
	}
	return s.handleOne(ctx, raw)
}

func (s *Server) handleOne(ctx context.Context, raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '[' {
		return protocolError(nil, -32600, "invalid request"), true
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return protocolError(nil, -32700, "parse error"), true
	}
	var call callParams
	if req.Method == "tools/call" && json.Unmarshal(req.Params, &call) == nil && call.Name == exportToolName {
		if len(req.ID) == 0 {
			return nil, false
		}
		summary, err := s.export(ctx, call.Arguments)
		if err != nil {
			return toolResponse(req.ID, map[string]any{"error": err.Error()}, true), true
		}
		return toolResponse(req.ID, summary, false), true
	}

	response, ok, err := s.proxy(ctx, raw)
	if err != nil {
		if len(req.ID) == 0 {
			return nil, false
		}
		return protocolError(req.ID, -32000, err.Error()), true
	}
	if !ok {
		return nil, false
	}
	if req.Method == "tools/list" {
		response = addExportTool(response)
	}
	if req.Method == "initialize" {
		response = addLocalInstructions(response)
	}
	return response, true
}

func (s *Server) proxy(ctx context.Context, body []byte) (json.RawMessage, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("reach LightShip: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyBody+1))
	if err != nil {
		return nil, false, fmt.Errorf("read LightShip response: %w", err)
	}
	if len(data) > maxProxyBody {
		return nil, false, errors.New("LightShip MCP response exceeded the local safety limit")
	}
	if resp.StatusCode == http.StatusAccepted && len(bytes.TrimSpace(data)) == 0 {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("LightShip returned %s", resp.Status)
	}
	if !json.Valid(data) {
		return nil, false, errors.New("LightShip returned an invalid MCP response")
	}
	return json.RawMessage(data), true, nil
}

func addExportTool(raw json.RawMessage) json.RawMessage {
	var response map[string]any
	if json.Unmarshal(raw, &response) != nil {
		return raw
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		return raw
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		return raw
	}
	result["tools"] = append(tools, exportTool())
	encoded, err := json.Marshal(response)
	if err != nil {
		return raw
	}
	return encoded
}

func addLocalInstructions(raw json.RawMessage) json.RawMessage {
	var response map[string]any
	if json.Unmarshal(raw, &response) != nil {
		return raw
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		return raw
	}
	current, _ := result["instructions"].(string)
	result["instructions"] = current + "\n\nThis connection includes a local export tool. Use " +
		exportToolName + " for analysis involving more than one or two traces. It writes JSONL " +
		"inside the workspace and returns only the file path and counts; do not read the whole file " +
		"into context. Analyse it with bounded local queries instead."
	encoded, err := json.Marshal(response)
	if err != nil {
		return raw
	}
	return encoded
}

func exportTool() map[string]any {
	return map[string]any{
		"name": exportToolName,
		"description": "Export every authorized span matching a time window and structured filter " +
			"to a local JSONL file for analysis. Pagination and atomic file creation are automatic. " +
			"The tool returns only paths and counts; trace rows never enter MCP context.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from":   map[string]any{"type": "string", "description": "RFC3339; defaults to 24h before to"},
				"to":     map[string]any{"type": "string", "description": "RFC3339; defaults to now"},
				"filter": map[string]any{"type": "object", "description": "Structured LightShip filter; conditions are ANDed"},
				"columns": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Columns to write; omit for every column"},
			},
			"additionalProperties": false,
		},
	}
}

func (s *Server) export(ctx context.Context, raw json.RawMessage) (exportSummary, error) {
	var args exportArgs
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return exportSummary{}, fmt.Errorf("invalid export arguments: %w", err)
		}
	}
	from, to, err := s.fixedWindow(args.From, args.To)
	if err != nil {
		return exportSummary{}, err
	}
	if err := os.MkdirAll(s.exportDir, 0o700); err != nil {
		return exportSummary{}, fmt.Errorf("create export directory: %w", err)
	}
	tmpDir, err := os.MkdirTemp(s.exportDir, ".partial-")
	if err != nil {
		return exportSummary{}, fmt.Errorf("create export: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return exportSummary{}, fmt.Errorf("protect export directory: %w", err)
	}
	rowsPath := filepath.Join(tmpDir, "traces.jsonl")
	rows, err := os.OpenFile(rowsPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return exportSummary{}, fmt.Errorf("create trace file: %w", err)
	}

	cursor := ""
	seen := map[string]bool{}
	pages, spans := 0, 0
	var binding json.RawMessage
	for {
		pageBinding, next, count, pageErr := s.exportPage(ctx, rows, args, from, to, cursor)
		if pageErr != nil {
			_ = rows.Close()
			return exportSummary{}, pageErr
		}
		if binding == nil {
			binding = pageBinding
		}
		pages++
		spans += count
		if next == "" {
			break
		}
		if seen[next] {
			_ = rows.Close()
			return exportSummary{}, errors.New("LightShip repeated an export cursor")
		}
		seen[next] = true
		cursor = next
	}
	if err := rows.Close(); err != nil {
		return exportSummary{}, fmt.Errorf("finish trace file: %w", err)
	}

	id, err := s.exportID()
	if err != nil {
		return exportSummary{}, err
	}
	finalDir := filepath.Join(s.exportDir, id)
	metadataPath := filepath.Join(tmpDir, "metadata.json")
	metadata := map[string]any{
		"export_id": id, "complete": true, "from": from, "to": to,
		"pages": pages, "spans": spans, "columns": args.Columns,
	}
	if len(args.Filter) > 0 && string(args.Filter) != "null" {
		metadata["filter"] = args.Filter
	}
	if len(binding) > 0 {
		metadata["binding"] = binding
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return exportSummary{}, fmt.Errorf("render export metadata: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(metadataPath, encoded, 0o600); err != nil {
		return exportSummary{}, fmt.Errorf("write export metadata: %w", err)
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return exportSummary{}, fmt.Errorf("publish export: %w", err)
	}
	complete = true
	return exportSummary{
		ExportID: id, Path: filepath.Join(finalDir, "traces.jsonl"),
		Metadata: filepath.Join(finalDir, "metadata.json"), Pages: pages, Spans: spans,
		From: from, To: to, Complete: true,
	}, nil
}

func (s *Server) fixedWindow(from, to string) (string, string, error) {
	var end time.Time
	var err error
	if to == "" {
		end = s.now().UTC()
		to = end.Format(time.RFC3339Nano)
	} else {
		end, err = time.Parse(time.RFC3339, to)
		if err != nil {
			return "", "", errors.New("to must be RFC3339")
		}
	}
	if from == "" {
		from = end.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	} else if _, err := time.Parse(time.RFC3339, from); err != nil {
		return "", "", errors.New("from must be RFC3339")
	}
	return from, to, nil
}

func (s *Server) exportPage(ctx context.Context, dst io.Writer, args exportArgs,
	from, to, cursor string) (json.RawMessage, string, int, error) {
	body := map[string]any{"from": from, "to": to, "limit": pageLimit}
	if len(args.Filter) > 0 && string(args.Filter) != "null" {
		body["filter"] = args.Filter
	}
	if len(args.Columns) > 0 {
		body["columns"] = args.Columns
	}
	if cursor != "" {
		body["cursor"] = cursor
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/traces/query",
		bytes.NewReader(encoded))
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", 0, fmt.Errorf("download traces: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(message, &failure)
		if failure.Error == "" {
			failure.Error = resp.Status
		}
		return nil, "", 0, fmt.Errorf("download traces: %s", failure.Error)
	}
	return decodePage(resp.Body, dst)
}

func decodePage(src io.Reader, dst io.Writer) (json.RawMessage, string, int, error) {
	dec := json.NewDecoder(src)
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, "", 0, errors.New("LightShip returned an invalid trace page")
	}
	var binding json.RawMessage
	next, count := "", 0
	foundSpans := false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, "", count, errors.New("LightShip returned an incomplete trace page")
		}
		switch key {
		case "binding":
			if err := dec.Decode(&binding); err != nil {
				return nil, "", count, errors.New("LightShip returned invalid binding metadata")
			}
		case "spans":
			open, err := dec.Token()
			if err != nil || open != json.Delim('[') {
				return nil, "", count, errors.New("LightShip returned an invalid spans array")
			}
			for dec.More() {
				var span json.RawMessage
				if err := dec.Decode(&span); err != nil {
					return nil, "", count, errors.New("LightShip returned an incomplete span")
				}
				if _, err := dst.Write(append(span, '\n')); err != nil {
					return nil, "", count, fmt.Errorf("write trace file: %w", err)
				}
				count++
			}
			if close, err := dec.Token(); err != nil || close != json.Delim(']') {
				return nil, "", count, errors.New("LightShip returned an incomplete spans array")
			}
			foundSpans = true
		case "next_cursor":
			if err := dec.Decode(&next); err != nil {
				return nil, "", count, errors.New("LightShip returned an invalid export cursor")
			}
		default:
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return nil, "", count, errors.New("LightShip returned an incomplete trace page")
			}
		}
	}
	if close, err := dec.Token(); err != nil || close != json.Delim('}') || !foundSpans {
		return nil, "", count, errors.New("LightShip returned an incomplete trace page")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, "", count, errors.New("LightShip returned trailing trace data")
	}
	return binding, next, count, nil
}

func (s *Server) exportID() (string, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("create export id: %w", err)
	}
	return "exp_" + s.now().UTC().Format("20060102T150405Z") + "_" + hex.EncodeToString(suffix[:]), nil
}

func protocolError(id json.RawMessage, code int, message string) json.RawMessage {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	encoded, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
	return encoded
}

func toolResponse(id json.RawMessage, body any, isError bool) json.RawMessage {
	text, err := json.Marshal(body)
	if err != nil {
		text = []byte(`{"error":"could not render result"}`)
		isError = true
	}
	encoded, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(text)}},
			"isError": isError,
		},
	})
	return encoded
}
