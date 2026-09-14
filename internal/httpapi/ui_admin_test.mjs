import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

// Run with: node --experimental-vm-modules --test internal/httpapi/ui_admin_test.mjs
class Element {
  children = [];
  style = {};
  append(...children) { this.children.push(...children); }
  appendChild(child) { this.append(child); }
  replaceChildren(...children) { this.children = children; }
  setAttribute(name, value) { this[name] = value; }
}

async function usersUI({ failRoles = false, failRefresh = false, roles = ["old-role"] } = {}) {
  const nodes = new Map();
  const $ = id => {
    if (!nodes.has(id)) nodes.set(id, new Element());
    return nodes.get(id);
  };
  const el = (tag, cls, text) => Object.assign(new Element(), { tag, className: cls, textContent: text });
  const stored = { username: "alice", roles: roles.slice(), attributes: { tenant: "managed-by-api" } };
  const calls = [];
  const requests = [];
  let loaded = false;
  const api = async (path, opts) => {
    calls.push([path, opts?.method || "GET"]);
    requests.push({ path, method: opts?.method || "GET", body: opts?.body });
    if (opts && path === "/users/alice")
      assert.equal($("users-rows").children[0].children[2].children[0].disabled, true);
    if (path === "/users") {
      if (opts?.method === "POST") return { username: JSON.parse(opts.body).username, password: "temporary" };
      if (loaded && failRefresh) throw new Error("refresh unavailable");
      loaded = true;
      return { users: [structuredClone(stored)] };
    }
    if (path === "/schema") return { model: { roles: ["old-role", "new-role", "admin"].map(name => ({ name })) } };
    if (path === "/users/alice") {
      if (failRoles) throw new Error("unknown role");
      stored.roles = JSON.parse(opts.body).roles;
      return {};
    }
    throw new Error(`unexpected request ${path}`);
  };
  const context = vm.createContext({ document: { createElement: tag => el(tag) } });
  const exports = {
    $, api, el, cell: text => el("td", "", text),
    API: { users: "/users", schema: "/schema", user: name => `/users/${name}` },
    setSetupProgress: () => {},
    viewErr: (id, error) => { $(id).textContent = error?.message || ""; },
  };
  const core = new vm.SyntheticModule(Object.keys(exports), function () {
    for (const [name, value] of Object.entries(exports)) this.setExport(name, value);
  }, { context });
  const roleBuilder = new vm.SourceTextModule(
    await readFile(new URL("./ui/role-builder.js", import.meta.url), "utf8"), { context });
  await roleBuilder.link(() => { throw new Error("role-builder.js has no imports"); });
  const admin = new vm.SourceTextModule(await readFile(new URL("./ui/admin.js", import.meta.url), "utf8"), { context });
  await admin.link(specifier => specifier === "./core.js" ? core : roleBuilder);
  await admin.evaluate();
  await admin.namespace.loadUsers();
  const controls = () => {
    const row = $("users-rows").children[0];
    return { roles: row.children[1].children[0], save: row.children[2].children[0] };
  };
  const original = controls();
  return { $, calls, requests, stored, controls, original };
}

const roleInput = (picker, name) => picker.roleInputs.find(input => input.value === name);

test("role controls expose every role as independent checkboxes", async () => {
  const ui = await usersUI();
  assert.equal(ui.original.roles.tag, "div");
  assert.deepEqual(JSON.parse(JSON.stringify(ui.original.roles.roleInputs.map(input => [input.value, input.checked]))),
    [["old-role", true], ["new-role", false], ["admin", false]]);
  assert.deepEqual(JSON.parse(JSON.stringify(ui.$("user-new-roles").roleInputs.map(input => input.value))),
    ["old-role", "new-role", "admin"]);
});

test("new users submit all selected roles without UI-managed attributes", async () => {
  const ui = await usersUI();
  ui.$("user-new-name").value = "bob";
  roleInput(ui.$("user-new-roles"), "old-role").checked = true;
  roleInput(ui.$("user-new-roles"), "new-role").checked = true;
  await ui.$("user-new").onclick();
  const request = ui.requests.find(item => item.path === "/users" && item.method === "POST");
  assert.deepEqual(JSON.parse(request.body), { username: "bob", roles: ["old-role", "new-role"] });
});

test("role failure is reported without changing the stored assignment", async () => {
  const ui = await usersUI({ failRoles: true });
  roleInput(ui.original.roles, "old-role").checked = false;
  roleInput(ui.original.roles, "new-role").checked = true;
  await ui.original.save.onclick();
  assert.deepEqual(ui.calls, [["/users", "GET"], ["/schema", "GET"], ["/users/alice", "PATCH"]]);
  assert.equal(ui.$("users-err").textContent, "Could not save roles for alice: unknown role");
  assert.deepEqual(ui.stored.roles, ["old-role"]);
  assert.equal(ui.original.save.disabled, false);
});

test("successful save replaces the role and refreshes the dropdown", async () => {
  const ui = await usersUI();
  roleInput(ui.original.roles, "old-role").checked = false;
  roleInput(ui.original.roles, "new-role").checked = true;
  await ui.original.save.onclick();
  assert.deepEqual(ui.calls, [["/users", "GET"], ["/schema", "GET"],
    ["/users/alice", "PATCH"], ["/users", "GET"], ["/schema", "GET"]]);
  assert.deepEqual(JSON.parse(JSON.stringify(ui.controls().roles.roleInputs
    .filter(input => input.checked).map(input => input.value))),
    ["new-role"]);
  assert.deepEqual(ui.stored.roles, ["new-role"]);
  assert.equal(ui.$("users-err").textContent, "");
});

test("an existing multi-role assignment is preserved on save", async () => {
  const ui = await usersUI({ roles: ["old-role", "new-role"] });
  await ui.original.save.onclick();
  assert.deepEqual(ui.stored.roles, ["old-role", "new-role"]);
});

test("successful role save followed by refresh failure is explicit", async () => {
  const ui = await usersUI({ failRefresh: true });
  roleInput(ui.original.roles, "old-role").checked = false;
  roleInput(ui.original.roles, "new-role").checked = true;
  await ui.original.save.onclick();
  assert.match(ui.$("users-err").textContent, /role was saved, but refreshing users failed/i);
});

async function traceQueryModule() {
  const context = vm.createContext({});
  const mod = new vm.SourceTextModule(
    await readFile(new URL("./ui/trace-query.js", import.meta.url), "utf8"), { context });
  await mod.link(() => { throw new Error("trace-query.js has no imports"); });
  await mod.evaluate();
  return mod.namespace;
}

async function dateRangeModule() {
  const nodes = new Map();
  const $ = id => {
    if (!nodes.has(id)) nodes.set(id, new Element());
    return nodes.get(id);
  };
  const context = vm.createContext({});
  const session = {demoWindow: null};
  const core = new vm.SyntheticModule(["$", "session"], function () {
    this.setExport("$", $);
    this.setExport("session", session);
  }, { context });
  const mod = new vm.SourceTextModule(
    await readFile(new URL("./ui/date-range.js", import.meta.url), "utf8"), { context });
  await mod.link(specifier => specifier === "./core.js" ? core : Promise.reject(
    new Error(`unexpected import ${specifier}`)));
  await mod.evaluate();
  return { $, session, range: mod.namespace };
}

test("shared date range supports presets and explicit custom bounds", async () => {
  const ui = await dateRangeModule();
  ui.$("preset").value = "24";
  ui.range.initDateRange("preset", "from", "to", "custom");
  assert.equal(ui.$("custom").hidden, true);
  const initial = ui.range.dateRange("from", "to");
  assert.equal((new Date(initial.to) - new Date(initial.from)) / 3600000, 24);
  ui.$("preset").value = "72";
  ui.$("preset").onchange();
  assert.equal(ui.$("custom").hidden, true);
  const wider = ui.range.dateRange("from", "to");
  assert.equal((new Date(wider.to) - new Date(wider.from)) / 3600000, 72);
  ui.$("from").value = "2020-01-01T00:00";
  ui.$("to").value = "2020-01-04T00:00";
  const refreshed = ui.range.dateRange("from", "to", "preset");
  assert.equal((new Date(refreshed.to) - new Date(refreshed.from)) / 3600000, 72);
  assert.ok(new Date(refreshed.to).getTime() > Date.now() - 5000);
  ui.session.demoWindow = {from: "2026-09-08T15:19:32Z", to: "2026-09-08T16:19:32Z"};
  ui.$("preset").value = "demo";
  ui.$("preset").onchange();
  assert.deepEqual(JSON.parse(JSON.stringify(ui.range.dateRange("from", "to", "preset"))),
    ui.session.demoWindow);
  ui.$("from").onchange();
  assert.equal(ui.$("preset").value, "custom");
  assert.equal(ui.$("custom").hidden, false);
});

async function roleBuilderModule() {
  const context = vm.createContext({});
  const mod = new vm.SourceTextModule(
    await readFile(new URL("./ui/role-builder.js", import.meta.url), "utf8"), { context });
  await mod.link(() => { throw new Error("role-builder.js has no imports"); });
  await mod.evaluate();
  return mod.namespace;
}

test("role builder creates policies from literals, booleans, and user attributes", async () => {
  const builder = await roleBuilderModule();
  const fields = Array.from(builder.roleFields({ fields: [
    { name: "TenantId", logical_type: "string", policy: true },
    { name: "IsError", logical_type: "boolean", policy: true },
    { name: "Tags", logical_type: "string_array", policy: true },
  ] }));
  const expression = builder.buildRolePolicy([
    { field: "TenantId", op: "eq", source: "user", value: "tenant_id" },
    { field: "IsError", op: "eq", source: "literal", value: "true" },
    { field: "Tags", op: "contains", source: "literal", value: "agent:pii" },
  ], fields, "all");
  assert.equal(expression, 'TenantId == user.tenant_id && IsError == true && "agent:pii" in Tags');
});

test("role builder parses existing simple policies without changing their meaning", async () => {
  const builder = await roleBuilderModule();
  const fields = Array.from(builder.roleFields({ fields: [
    { name: "TenantId", logical_type: "string", policy: true },
    { name: "Tags", logical_type: "string_array", policy: true },
  ] }));
  const parsed = builder.parseRolePolicies([{ expression:
    'TenantId == user.tenant_id && "agent:pii" in Tags' }], fields, ["tenant_id"]);
  assert.deepEqual(JSON.parse(JSON.stringify(parsed)), { match: "all", conditions: [
    { field: "TenantId", op: "eq", source: "user", value: "tenant_id" },
    { field: "Tags", op: "contains", source: "literal", value: "agent:pii" },
  ] });
  assert.equal(builder.parseRolePolicies([{ expression: "TenantId == user.tenant_id && true || false" }],
    fields, ["tenant_id"]), null);
});

test("roles stay read-only until Edit opens the visual builder", async () => {
  const nodes = new Map();
  const $ = id => {
    if (!nodes.has(id)) nodes.set(id, new Element());
    return nodes.get(id);
  };
  const el = (tag, cls, text) => Object.assign(new Element(), { tag, className: cls, textContent: text });
  const model = {
    fields: [{ name: "TenantId", logical_type: "string", policy: true }],
    user_attributes: ["tenant_id"],
    roles: [
      { name: "admin", policies: [{ expression: "true" }] },
      { name: "support", policies: [{ expression: "TenantId == user.tenant_id" }] },
    ],
  };
  const api = async path => path === "/schema" ? { model } : Promise.reject(new Error(`unexpected request ${path}`));
  const context = vm.createContext({ document: { createElement: tag => el(tag) } });
  const exports = {
    $, api, el, cell: text => el("td", "", text),
    API: { schema: "/schema", roles: name => `/roles/${name}` }, setSetupProgress: () => {},
    viewErr: () => {},
  };
  const core = new vm.SyntheticModule(Object.keys(exports), function () {
    for (const [name, value] of Object.entries(exports)) this.setExport(name, value);
  }, { context });
  const roleBuilder = new vm.SourceTextModule(
    await readFile(new URL("./ui/role-builder.js", import.meta.url), "utf8"), { context });
  await roleBuilder.link(() => { throw new Error("role-builder.js has no imports"); });
  const admin = new vm.SourceTextModule(
    await readFile(new URL("./ui/admin.js", import.meta.url), "utf8"), { context });
  await admin.link(specifier => specifier === "./core.js" ? core : roleBuilder);
  await admin.evaluate();
  await admin.namespace.loadRoles();

  const descendants = node => [node, ...node.children.filter(child => child instanceof Element)
    .flatMap(descendants)];
  assert.equal($("roles-list").children.length, 2);
  assert.equal(descendants($("roles-list")).some(node => node.tag === "textarea"), false);
  const supportCard = $("roles-list").children[1];
  assert.equal(descendants(supportCard).find(node => node.className === "role-summary").textContent,
    "TenantId matches user’s tenant_id");
  assert.equal(descendants(supportCard).find(node => node.className === "expr").textContent,
    "TenantId == user.tenant_id");
  const edit = supportCard.children[0].children[1];
  assert.equal(edit.textContent, "Edit");
  edit.onclick();
  const inlineBuilder = $("roles-list").children[1];
  assert.match(inlineBuilder.className, /role-builder/);
  assert.equal(descendants(inlineBuilder).some(node => node.tag === "select"), true);
});

test("trace list projection contains only structural columns", async () => {
  const query = await traceQueryModule();
  const schema = {
    binding: { trace_id: "TraceId", timestamp: "Timestamp", span_id: "SpanId",
      parent_span_id: "ParentSpanId", name: "SpanName" },
    fields: [
      { name: "ServiceName", type: "string", operators: ["eq"] },
      { map: "SpanAttributes", name: "tenant.id", type: "string", operators: ["eq"] },
      { map: "SpanAttributes", name: "tags", type: "string_array", operators: ["has"] },
      { name: "PolicyOnly", type: "string" },
    ],
  };
  assert.deepEqual(Array.from(query.queryColumns(schema)), ["TraceId", "Timestamp", "SpanId",
    "ParentSpanId", "SpanName"]);
});

test("filter drafts preserve typed values and structured wire operators", async () => {
  const query = await traceQueryModule();
  assert.equal(query.operatorLabel("eq"), "==");
  assert.equal(query.operatorLabel("has_any"), "hasAny()");
  const plain = value => JSON.parse(JSON.stringify(value));
  assert.deepEqual(plain(query.conditionFromDraft({name:"score", type:"number"}, "between", "1, 2.5")),
    {name:"score", op:"between", values:[1, 2.5]});
  assert.deepEqual(plain(query.conditionFromDraft({map:"Attrs", name:"tags", type:"string_array"},
    "has", "agent:pii")), {map:"Attrs", name:"tags", op:"has", value:"agent:pii"});
  assert.deepEqual(plain(query.conditionFromDraft({name:"sampled", type:"boolean"}, "eq", "false")),
    {name:"sampled", op:"eq", value:false});
});

test("ClickHouse field types are inferred for read-only display", async () => {
  const query = await traceQueryModule();
  assert.equal(query.logicalTypeForClickHouse("LowCardinality(String)"), "string");
  assert.equal(query.logicalTypeForClickHouse("Nullable(Bool)"), "boolean");
  assert.equal(query.logicalTypeForClickHouse("UInt64"), "number");
  assert.equal(query.logicalTypeForClickHouse("Array(String)"), "string_array");
});
