import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

class Element {
  constructor(tag = "div") { this.tag = tag; }
  children = [];
  dataset = {};
  hidden = false;
  style = { setProperty: () => {} };
  listeners = {};
  append(...children) { this.children.push(...children); }
  appendChild(child) { this.append(child); return child; }
  replaceChildren(...children) { this.children = children; }
  setAttribute(name, value) { this[name] = value; }
  addEventListener(name, handler) { this.listeners[name] = handler; }
}

const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};

const binding = {
  trace_id: "TraceId", timestamp: "Timestamp", span_id: "SpanId",
  parent_span_id: "ParentSpanId", name: "SpanName",
};
const trace = (id = "trace-1") => ({ spans: [{
  TraceId: id, Timestamp: "2026-01-01T00:00:00Z", SpanId: "span-1",
  ParentSpanId: "", SpanName: "root operation", TenantId: "cedar", Extra: "shown",
}], binding, next_cursor: null });

async function tracesUI({demoEnabled = true} = {}) {
  const nodes = new Map();
  const $ = id => {
    if (!nodes.has(id)) nodes.set(id, new Element());
    return nodes.get(id);
  };
  $("view-list").hidden = false;
  $("view-detail").hidden = true;
  const nav = new Element("button");
  nav.dataset.view = "keys";
  const queryRequests = [];
  const detailRequests = [];
  const api = (path, options) => {
    if (path === "/filter/schema") return Promise.resolve({
      binding, fields: [{name: "TraceId", type: "string", operators: ["eq"]},
        {name: "TenantId", type: "string", operators: ["eq"]}],
    });
    const request = {...deferred(), options};
    options.signal?.addEventListener("abort", () => request.reject(new Error("aborted")));
    if (path === "/traces/query") queryRequests.push(request);
    else if (path.startsWith("/traces/")) detailRequests.push(request);
    else return Promise.reject(new Error(`unexpected request ${path}`));
    return request.promise;
  };
  const document = {
    createElement: tag => new Element(tag),
    querySelectorAll: () => [nav],
  };
  let nextTimer = 0;
  const timers = new Map();
  const setTimeout = (callback, delay) => {
    const id = ++nextTimer;
    timers.set(id, {callback, delay});
    return id;
  };
  const clearTimeout = id => timers.delete(id);
  const runNextTimer = () => {
    const next = timers.entries().next();
    assert.equal(next.done, false, "no pending retry timer");
    const [id, timer] = next.value;
    timers.delete(id);
    timer.callback();
    return timer.delay;
  };
  const context = vm.createContext({ AbortController, clearTimeout, document, queueMicrotask,
    setTimeout });
  const coreExports = {
    $, API: { filterSchema: "/filter/schema", traceQuery: "/traces/query",
      trace: id => `/traces/${id}` }, api,
    session: {demoEnabled},
    cell: (text, cls) => Object.assign(new Element("td"), {textContent: text, className: cls || ""}),
    el: (tag, cls, text) => Object.assign(new Element(tag), {className: cls, textContent: text}),
  };
  const core = new vm.SyntheticModule(Object.keys(coreExports), function () {
    for (const [name, value] of Object.entries(coreExports)) this.setExport(name, value);
  }, {context});
  const queryExports = {
    conditionFromDraft: () => ({}), fieldId: field => field.name,
    fieldRef: field => field.name, filterableFields: schema => schema.fields || [],
    operatorLabel: name => name, queryColumns: schema => Object.values(schema.binding),
  };
  const query = new vm.SyntheticModule(Object.keys(queryExports), function () {
    for (const [name, value] of Object.entries(queryExports)) this.setExport(name, value);
  }, {context});
  const rangeExports = {
    dateRange: () => ({from: "2025-12-31T00:00:00Z", to: "2026-01-02T00:00:00Z"}),
    initDateRange: () => {},
  };
  const range = new vm.SyntheticModule(Object.keys(rangeExports), function () {
    for (const [name, value] of Object.entries(rangeExports)) this.setExport(name, value);
  }, {context});
  const mod = new vm.SourceTextModule(
    await readFile(new URL("./ui/traces.js", import.meta.url), "utf8"), {context});
  await mod.link(specifier => ({"./core.js": core, "./trace-query.js": query,
    "./date-range.js": range})[specifier]);
  await mod.evaluate();
  return {$, nav, queryRequests, detailRequests, runNextTimer, timers, traces: mod.namespace};
}

const until = async predicate => {
  for (let attempt = 0; attempt < 20 && !predicate(); attempt++)
    await new Promise(resolve => setImmediate(resolve));
  assert.equal(predicate(), true, "timed out waiting for asynchronous trace work");
};

test("reset aborts and discards a stale trace list response", async () => {
  const ui = await tracesUI();
  const firstLoad = ui.traces.load();
  await until(() => ui.queryRequests.length === 1);
  assert.equal(ui.queryRequests.length, 1);
  assert.equal(ui.queryRequests[0].options.preview, true);
  ui.traces.reset();
  assert.equal(ui.queryRequests[0].options.signal.aborted, true);
  ui.queryRequests[0].resolve(trace("stale"));
  await firstLoad;
  assert.equal(ui.$("rows").children.length, 0);

  const currentLoad = ui.traces.load();
  await until(() => ui.queryRequests.length === 2);
  ui.queryRequests[1].resolve(trace("current"));
  await currentLoad;
  assert.equal(ui.$("rows").children.length, 1);
  assert.equal(ui.$("rows").children[0].children[2].textContent, "cedar");
  assert.equal(ui.$("rows").children[0].children[5].textContent, "current");
});

test("normal deployments omit the demo tenant projection and column", async () => {
  const ui = await tracesUI({demoEnabled: false});
  const load = ui.traces.load();
  await until(() => ui.queryRequests.length === 1);
  const body = JSON.parse(ui.queryRequests[0].options.body);
  assert.equal(body.columns.includes("TenantId"), false);
  ui.queryRequests[0].resolve(trace());
  await load;
  const row = ui.$("rows").children[0];
  assert.equal(row.children.length, 5);
  assert.equal(row.children[4].textContent, "trace-1");
});

test("Load more follows every cursor with fixed search bounds and a clear end state", async () => {
  const ui = await tracesUI();
  const firstLoad = ui.traces.load();
  await until(() => ui.queryRequests.length === 1);
  const first = trace("trace-3");
  first.next_cursor = "cursor-1";
  ui.queryRequests[0].resolve(first);
  await firstLoad;

  assert.equal(ui.$("load-more").hidden, false);
  assert.equal(ui.$("load-more").textContent, "Load more");
  assert.equal(ui.$("sentinel").textContent, "More traces are available.");
  const firstBody = JSON.parse(ui.queryRequests[0].options.body);
  assert.equal(firstBody.cursor, undefined);

  const secondLoad = ui.$("load-more").onclick();
  await until(() => ui.queryRequests.length === 2);
  assert.equal(ui.$("load-more").disabled, true);
  assert.equal(ui.$("load-more").textContent, "Loading…");
  const secondBody = JSON.parse(ui.queryRequests[1].options.body);
  assert.equal(secondBody.cursor, "cursor-1");
  assert.equal(secondBody.from, firstBody.from);
  assert.equal(secondBody.to, firstBody.to);
  assert.deepEqual(secondBody.filter, firstBody.filter);

  ui.queryRequests[1].resolve(trace("trace-2"));
  await secondLoad;
  assert.deepEqual(ui.$("rows").children.map(row => row.children[5].textContent),
    ["trace-3", "trace-2"]);
  assert.equal(ui.$("load-more").hidden, true);
  assert.equal(ui.$("sentinel").textContent, "end of results");
});

test("a failed next page can be retried with the same cursor", async () => {
  const ui = await tracesUI();
  const firstLoad = ui.traces.load();
  await until(() => ui.queryRequests.length === 1);
  const first = trace("trace-2");
  first.next_cursor = "cursor-1";
  ui.queryRequests[0].resolve(first);
  await firstLoad;

  const failedLoad = ui.$("load-more").onclick();
  await until(() => ui.queryRequests.length === 2);
  ui.queryRequests[1].reject(Object.assign(new Error("invalid filter"), {status: 400}));
  await failedLoad;
  assert.equal(ui.$("load-more").hidden, false);
  assert.equal(ui.$("load-more").disabled, false);
  assert.equal(ui.$("load-more").textContent, "Retry");
  assert.equal(ui.$("filter-error").textContent, "invalid filter");

  const retry = ui.$("load-more").onclick();
  await until(() => ui.queryRequests.length === 3);
  assert.equal(JSON.parse(ui.queryRequests[2].options.body).cursor, "cursor-1");
  ui.queryRequests[2].resolve(trace("trace-1"));
  await retry;
  assert.deepEqual(ui.$("rows").children.map(row => row.children[5].textContent),
    ["trace-2", "trace-1"]);
});

test("a transient trace failure retries automatically and renders after recovery", async () => {
  const ui = await tracesUI();
  const firstLoad = ui.traces.load();
  await until(() => ui.queryRequests.length === 1);
  const first = trace("trace-2");
  first.next_cursor = "cursor-1";
  ui.queryRequests[0].resolve(first);
  await firstLoad;

  const failedLoad = ui.$("load-more").onclick();
  await until(() => ui.queryRequests.length === 2);
  ui.queryRequests[1].reject(Object.assign(new Error("trace store unavailable"), {status: 502}));
  await failedLoad;
  assert.equal(ui.$("sentinel").textContent,
    "Trace source unavailable. Retrying automatically…");
  assert.equal(ui.$("load-more").hidden, true);
  assert.equal(ui.runNextTimer(), 2000);

  await until(() => ui.queryRequests.length === 3);
  assert.equal(JSON.parse(ui.queryRequests[2].options.body).cursor, "cursor-1");
  ui.queryRequests[2].resolve(trace("trace-1"));
  await until(() => ui.$("rows").children.length === 2);
  assert.deepEqual(ui.$("rows").children.map(row => row.children[5].textContent),
    ["trace-2", "trace-1"]);
  assert.equal(ui.$("sentinel").textContent, "end of results");
});

test("a trace request that remains pending enters automatic recovery", async () => {
  const ui = await tracesUI();
  const pendingLoad = ui.traces.load();
  await until(() => ui.queryRequests.length === 1);
  assert.equal(ui.runNextTimer(), 35000);
  await pendingLoad;
  assert.equal(ui.queryRequests[0].options.signal.aborted, true);
  assert.equal(ui.$("sentinel").textContent,
    "Trace source unavailable. Retrying automatically…");

  assert.equal(ui.runNextTimer(), 2000);
  await until(() => ui.queryRequests.length === 2);
  ui.queryRequests[1].resolve(trace("recovered"));
  await until(() => ui.$("rows").children.length === 1);
  assert.equal(ui.$("rows").children[0].children[5].textContent, "recovered");
});

test("trace rows expose a keyboard link and Back rejects a late detail response", async () => {
  const ui = await tracesUI();
  const load = ui.traces.load();
  await until(() => ui.queryRequests.length === 1);
  ui.queryRequests[0].resolve(trace());
  await load;
  const row = ui.$("rows").children[0];
  const link = row.children[1].children[0];
  assert.equal(link.tag, "a");
  assert.match(link["aria-label"], /Open trace trace-1/);

  const opening = row.onclick();
  assert.equal(ui.detailRequests[0].options.preview, true);
  assert.equal(ui.$("view-detail").hidden, false);
  assert.equal(ui.$("spans").children[0].textContent, "Loading trace…");
  ui.$("back").onclick();
  assert.equal(ui.detailRequests[0].options.signal.aborted, true);
  ui.detailRequests[0].resolve(trace());
  await opening;
  assert.equal(ui.$("view-detail").hidden, true);
  assert.equal(ui.$("view-list").hidden, false);
  assert.equal(ui.$("spans").children[0].textContent, "Loading trace…");
});

test("trace detail reports request failures in the visible detail view", async () => {
  const ui = await tracesUI();
  const load = ui.traces.load();
  await until(() => ui.queryRequests.length === 1);
  ui.queryRequests[0].resolve(trace());
  await load;
  const opening = ui.$("rows").children[0].onclick();
  ui.detailRequests[0].reject(new Error("backend unavailable"));
  await opening;
  const status = ui.$("spans").children[0];
  assert.equal(status.role, "alert");
  assert.equal(status.textContent, "Could not load trace: backend unavailable");
});

test("leaving Traces aborts an in-flight list request", async () => {
  const ui = await tracesUI();
  const load = ui.traces.load();
  await until(() => ui.queryRequests.length === 1);
  ui.nav.listeners.click();
  assert.equal(ui.queryRequests[0].options.signal.aborted, true);
  ui.queryRequests[0].resolve(trace("stale"));
  await load;
  assert.equal(ui.$("rows").children.length, 0);
});
