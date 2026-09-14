package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIJavaScriptModulesAreServed(t *testing.T) {
	h := New(nil, nil, nil, slog.Default())
	for _, name := range []string{"core.js", "date-range.js", "trace-query.js", "role-builder.js", "traces.js", "setup.js", "credentials.js", "admin.js", "app.js"} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/"+name, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /ui/%s = %d", name, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
				t.Fatalf("Content-Type = %q", got)
			}
			if rec.Body.Len() == 0 {
				t.Fatal("empty module")
			}
		})
	}
}

func TestUIStylesheetsAreServed(t *testing.T) {
	h := New(nil, nil, nil, slog.Default())
	for _, name := range []string{"design-system.css", "primer-light.css", "primer-core.css", "theme-primer.css"} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/"+name, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /ui/%s = %d", name, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
				t.Fatalf("Content-Type = %q", got)
			}
			if rec.Body.Len() == 0 {
				t.Fatal("empty stylesheet")
			}
		})
	}
}

func TestUIFontAssetsAreServed(t *testing.T) {
	h := New(nil, nil, nil, slog.Default())
	for _, name := range []string{"instrument-sans-latin.woff2", "geist-mono-latin.woff2"} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/"+name, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /ui/%s = %d", name, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "font/woff2" {
				t.Fatalf("Content-Type = %q", got)
			}
			if rec.Body.Len() == 0 {
				t.Fatal("empty font")
			}
		})
	}
}

func TestUIRejectsUnsupportedAssets(t *testing.T) {
	h := New(nil, nil, nil, slog.Default())
	for _, target := range []string{
		"/ui/index.html", "/ui/primer-LICENSES.txt", "/ui/missing.js", "/ui/missing.css",
		"/ui/nested/theme.css",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", target, rec.Code)
		}
	}
}

func TestUICSPAllowsExternalModulesNotInlineScripts(t *testing.T) {
	h := New(nil, nil, nil, slog.Default())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP does not allow same-origin modules: %q", csp)
	}
	if strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Fatalf("CSP still allows inline scripts: %q", csp)
	}
	if !strings.Contains(csp, "style-src 'self'") {
		t.Fatalf("CSP does not allow same-origin stylesheets: %q", csp)
	}
	if !strings.Contains(csp, "font-src 'self'") {
		t.Fatalf("CSP does not allow same-origin fonts: %q", csp)
	}
}

func TestUIUsesModuleEntryPoint(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `<script type="module" src="/ui/app.js"></script>`) {
		t.Fatal("index.html does not load the module entry point")
	}
	if got := strings.Count(html, "<script"); got != 1 {
		t.Fatalf("index.html contains %d script elements, want 1", got)
	}
	if strings.Contains(html, "<script>") {
		t.Fatal("inline JavaScript returned; keep behavior in the tested modules")
	}

	entry, err := fs.ReadFile(uiFS, "ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"core.js", "traces.js", "setup.js", "credentials.js", "admin.js"} {
		if !strings.Contains(string(entry), `from "./`+name+`"`) {
			t.Fatalf("app.js does not import %s", name)
		}
	}
}

func TestUIOffersConfiguredPublicDemoCredentials(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `id="demo-username"`) ||
		!strings.Contains(html, `id="demo-password"`) {
		t.Fatal("public demo credential placeholders are missing")
	}
	for _, want := range []string{`id="demo-entry" data-demo-only hidden`,
		`value="demo" data-demo-window-only hidden disabled`} {
		if !strings.Contains(html, want) {
			t.Errorf("demo presentation marker is missing %q", want)
		}
	}

	core, err := fs.ReadFile(uiFS, "ui/core.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`demo: "/demo"`, `document.querySelectorAll("[data-demo-only]")`,
		`document.querySelectorAll("[data-demo-window-only]")`} {
		if !strings.Contains(string(core), want) {
			t.Errorf("demo initializer is missing %q", want)
		}
	}
}

func TestUIUsesSwappableDesignSystemEntryPoint(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, `<link rel="stylesheet" href="/ui/design-system.css">`) {
		t.Fatal("index.html does not load the design-system entry point")
	}
	if strings.Contains(html, `href="/ui/primer-core.css"`) || strings.Contains(html, `href="/ui/theme-primer.css"`) {
		t.Fatal("index.html bypasses the swappable design-system entry point")
	}

	entry, err := fs.ReadFile(uiFS, "ui/design-system.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(entry)
	for _, want := range []string{`primer-light.css`, `primer-core.css`, `theme-primer.css`} {
		if !strings.Contains(css, want) {
			t.Fatalf("design-system.css does not load %s", want)
		}
	}

	theme, err := fs.ReadFile(uiFS, "ui/theme-primer.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`url("/ui/instrument-sans-latin.woff2")`, `url("/ui/geist-mono-latin.woff2")`} {
		if !strings.Contains(string(theme), want) {
			t.Fatalf("theme-primer.css does not load %s", want)
		}
	}
	for _, want := range []string{`.wiz {`, `.tabs .tab {`, `button.primary {`} {
		if !strings.Contains(string(theme), want) {
			t.Fatalf("theme-primer.css does not include shared component %s", want)
		}
	}
	if strings.Contains(string(theme), `.tabs .tab::before`) {
		t.Fatal("theme-primer.css still adds navigation dots")
	}
}

func TestCredentialControlsAppearOnlyOnCredentialsView(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if got := strings.Count(html, `id="mintkit"`); got != 1 {
		t.Fatalf("index.html contains %d credential controls, want 1", got)
	}
	if strings.Contains(html, `schema-mintslot`) {
		t.Fatal("Setup still contains credential controls")
	}
	credentials := strings.Index(html, `id="view-keys"`)
	mint := strings.Index(html, `id="mintkit"`)
	roles := strings.Index(html, `id="view-roles"`)
	if credentials < 0 || mint < credentials || roles < mint {
		t.Fatal("credential controls are not contained in the Credentials view")
	}
}

func TestUICoreOwnsSharedAPIAndBindingContracts(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/core.js")
	if err != nil {
		t.Fatal(err)
	}
	core := string(body)
	for _, want := range []string{
		`traceQuery: "/traces/query"`,
		`filterSchema: "/filter/schema"`,
		`discover: "/schema/discover"`,
		`binding: "/schema/binding"`,
		`fields: "/schema/fields"`,
		`"trace_id", "timestamp", "span_id", "parent_span_id", "name"`,
	} {
		if !strings.Contains(core, want) {
			t.Fatalf("core.js is missing shared contract %q", want)
		}
	}
}

func TestUIUsesOneDateRangePatternForTracesAndFieldDiscovery(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`id="trace-range-preset"`, `id="from" type="datetime-local"`,
		`id="to" type="datetime-local"`, `id="trace-custom-range" hidden`,
		`id="flds-range-preset"`, `id="flds-custom-range" hidden`,
		`id="flds-from" type="datetime-local"`, `id="flds-to" type="datetime-local"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("shared date range UI is missing %q", want)
		}
	}
	for _, module := range []string{"traces.js", "setup.js"} {
		body, err := fs.ReadFile(uiFS, "ui/"+module)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `from "./date-range.js"`) {
			t.Errorf("%s does not use the shared date range", module)
		}
	}
}

func TestUITraceDetailRequestsAndRendersCompleteRows(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/traces.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	if !strings.Contains(js, `data = await api(API.trace(id), {signal: controller.signal, preview: true});`) {
		t.Error("trace detail still sends a field projection")
	}
	if !strings.Contains(js, `Object.entries(span)`) {
		t.Error("trace detail does not render the complete returned row")
	}
	if !strings.Contains(js, `document.createElement("details")`) {
		t.Error("trace spans are not collapsible details")
	}
	if strings.Contains(js, `no filterable fields`) {
		t.Error("trace detail still describes attributes as filterable-only")
	}
}

func TestUITraceListShowsTenantOnlyInDemoMode(t *testing.T) {
	index, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `<th data-demo-only hidden>tenant</th>`) {
		t.Error("trace list tenant heading is not demo-gated")
	}

	body, err := fs.ReadFile(uiFS, "ui/traces.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`if (!session.demoEnabled) return null;`,
		`field.name === "TenantId"`,
		`if (tenant) columns.push(tenant);`,
		`if (session.demoEnabled) cells.push(cell(trace.tenant || "—"));`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("trace list tenant rendering is missing %q", want)
		}
	}
}

func TestUIRetriesTransientTraceFailures(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/traces.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)
	for _, want := range []string{
		`const transientStatuses = new Set([429, 502, 503, 504]);`,
		`if (error.message === "unauthenticated")`,
		`Trace source unavailable. Retrying automatically…`,
		`const requestTimeout = 35000;`,
		`controller.abort();`,
		`scheduleRetry(generation);`,
		`retryDelay = Math.min(retryDelay * 2, maxRetryDelay);`,
		`clearTimeout(retryTimer);`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("trace retry behavior is missing %q", want)
		}
	}
}

func TestUIExposesAdminRolePreview(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="role-preview-label"`, `id="role-preview"`, `Preview role`,
		`id="preview-banner"`, `id="preview-exit"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("role preview UI is missing %q", want)
		}
	}
	core, err := fs.ReadFile(uiFS, "ui/core.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(core), `X-LightShip-Preview-Role`) {
		t.Error("API requests do not carry the selected preview role")
	}
	if !strings.Contains(string(core), `if (preview && session.previewRole)`) {
		t.Error("role preview is not scoped to explicitly previewable requests")
	}
}

func TestUIExposesSchemaDrivenTraceAndPolicyControls(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`id="filter-add"`, `id="filter-rows"`, `id="filter-error"`,
		`Connect data`, `Define roles`, `Manage users`,
		`id="role-new"`, `id="flds-add">+ Add field`, `Connect trace data`,
		`id="user-new-roles"`, `<th>username</th><th>roles</th>`,
		`https://lightship.mintlifysite.com/docs/concepts/policies`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
	for _, removed := range []string{
		`data-sort="coverage"`, `data-sort="distinct"`, `data-sort="spans"`,
		`id="flds-addmap"`, `id="flds-addname"`, `id="ua-chips"`, `id="ua-new"`,
		`id="policy-guide"`, `id="role-new-name"`, `id="role-attrs"`, `id="role-builder"`,
		`id="user-new-role"`, `id="user-new-attrs"`, `attributes JSON`,
	} {
		if strings.Contains(html, removed) {
			t.Errorf("index.html still contains removed field control %q", removed)
		}
	}
}

func TestUIUsesCompactSetupNavigation(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	wants := []string{
		`class="sidebar"`, `aria-label="Primary navigation"`, `data-setup-root`,
		`class="subnav"`,
		`data-view="schema"`, `data-view="roles"`, `data-view="users"`,
		`Connect data`, `Define roles`, `Manage users`,
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
	for _, removed := range []string{
		`id="setup-guide"`, `class="steps"`,
		`Set up LightShip`, `Complete these steps in order`, `Step 1 of 3`, `Step 2 of 3`,
		`Step 3 of 3`, `class="step-number"`, `class="step-copy"`,
		`Configure trace data`, `Review the access already configured for your team`,
	} {
		if strings.Contains(html, removed) {
			t.Errorf("index.html still contains redundant setup content %q", removed)
		}
	}
}

func TestConnectOrganizesCredentialsAndMCPSetup(t *testing.T) {
	body, err := fs.ReadFile(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{`>Connect</button>`, `<h1>Connect</h1>`,
		`<h2>API keys</h2>`, `<h2>Connect an AI client</h2>`, `class="card connect-group"`,
		`https://lightship.mintlifysite.com/use/mcp`,
		`Quick connection and advanced local-companion guide →`, `<h2>Password</h2>`} {
		if !strings.Contains(html, want) {
			t.Errorf("credentials view is missing %q", want)
		}
	}
	for _, removed := range []string{`<h1>MCP</h1>`, `<h1>Credentials</h1>`, `id="connect"`} {
		if strings.Contains(html, removed) {
			t.Errorf("credentials view still contains %q", removed)
		}
	}

	credentials, err := fs.ReadFile(uiFS, "ui/credentials.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{`--transport stdio`, `.mcp.json`, `renderConnect`} {
		if strings.Contains(string(credentials), removed) {
			t.Errorf("credentials.js still contains embedded setup content %q", removed)
		}
	}
}

func TestPublicDemoKeepsConnectAndUsesExpiringKeys(t *testing.T) {
	app, err := fs.ReadFile(uiFS, "ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(app), `session.isDemo`) &&
		strings.Contains(string(app), `tab.hidden = !!session.previewRole || session.isDemo`) {
		t.Fatal("public demo still hides the Connect tab")
	}
	credentials, err := fs.ReadFile(uiFS, "ui/credentials.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`session.demoEnabled ? "24h"`, `$("key-expiry").disabled = session.demoEnabled`,
		`$("password-section").hidden = session.isDemo`} {
		if !strings.Contains(string(credentials), want) {
			t.Errorf("demo MCP setup is missing %q", want)
		}
	}
}
