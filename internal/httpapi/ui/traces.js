import { $, API, api, cell, el, session } from "./core.js";
import { conditionFromDraft, fieldId, fieldRef, filterableFields, operatorLabel, queryColumns } from "./trace-query.js";
import { dateRange, initDateRange } from "./date-range.js";

let cursor = null, loading = false, done = false, schema = null, listSearch = null;
let listGeneration = 0, listController = null;
let detailGeneration = 0, detailController = null;
let retryTimer = null, retryDelay = 2000;
const maxRetryDelay = 10000;
const requestTimeout = 35000;
const transientStatuses = new Set([429, 502, 503, 504]);
const filters = [];

export function initWindow() {
  initDateRange("trace-range-preset", "from", "to", "trace-custom-range");
  for (const id of ["trace-range-preset", "from", "to"])
    $(id).addEventListener("change", refreshWindow);
}

function refreshWindow() {
  if ($("trace-range-preset").value === "custom") {
    if (!$('from').value || !$('to').value) return;
    try { dateRange("from", "to", "trace-range-preset"); } catch (_) { return; }
  }
  reset(); load();
}

function windowParams() {
  return dateRange("from", "to", "trace-range-preset");
}

export function reset() {
  listGeneration++;
  listController?.abort();
  listController = null;
  clearTimeout(retryTimer);
  retryTimer = null;
  retryDelay = 2000;
  loading = false;
  detailGeneration++;
  detailController?.abort();
  detailController = null;
  cursor = null;
  done = false;
  schema = null;
  listSearch = null;
  $("rows").replaceChildren();
  $("load-more").hidden = true;
  $("load-more").disabled = false;
  $("load-more").textContent = "Load more";
}

function scheduleRetry(generation) {
  clearTimeout(retryTimer);
  const delay = retryDelay;
  retryDelay = Math.min(retryDelay * 2, maxRetryDelay);
  retryTimer = setTimeout(() => {
    retryTimer = null;
    if (generation === listGeneration && !$("view-list").hidden && !loading && !done) load();
  }, delay);
}

async function ensureSchema(signal, generation = listGeneration) {
  if (!schema) {
    const loaded = await api(API.filterSchema, {signal, preview: true});
    if (signal?.aborted || generation !== listGeneration) return null;
    schema = loaded;
    renderFilters();
  }
  return schema;
}

const findField = id => filterableFields(schema || {}).find(field => fieldId(field) === id);

function valueControl(draft, field) {
  if (draft.op === "exists" || draft.op === "not_exists")
    return el("span", "faint filter-no-value", "no value");
  if (field.type === "boolean") {
    if (!draft.raw) draft.raw = "true";
    const select = document.createElement("select");
    select.setAttribute("aria-label", "filter value");
    select.append(new Option("true", "true"), new Option("false", "false"));
    select.value = draft.raw;
    select.onchange = () => { draft.raw = select.value; reset(); load(); };
    return select;
  }
  const input = document.createElement("input");
  input.setAttribute("aria-label", "filter value");
  input.value = draft.raw || "";
  input.type = field.type === "number" && draft.op !== "between" ? "number" : "text";
  input.placeholder = ["in", "not_in", "has_any", "has_all", "between"].includes(draft.op)
    ? "comma-separated values" : (field.type === "number" ? "number" : "value");
  input.oninput = () => { draft.raw = input.value; };
  input.onchange = () => { reset(); load(); };
  return input;
}

function renderFilters() {
  const fields = filterableFields(schema || {});
  const host = $("filter-rows");
  host.replaceChildren();
  $("filter-add").disabled = !fields.length;
  $("filter-empty").hidden = !!filters.length;
  for (const draft of filters) {
    let field = findField(draft.field);
    if (!field) { field = fields[0]; draft.field = fieldId(field); }
    if (!field.operators.includes(draft.op)) draft.op = field.operators[0];

    const row = el("div", "filter-row");
    const fieldSelect = document.createElement("select");
    fieldSelect.setAttribute("aria-label", "filter field");
    for (const f of fields)
      fieldSelect.appendChild(new Option(`${fieldRef(f)} · ${f.type}`, fieldId(f)));
    fieldSelect.value = draft.field;
    fieldSelect.onchange = () => {
      draft.field = fieldSelect.value;
      draft.op = findField(draft.field).operators[0];
      draft.raw = "";
      renderFilters(); reset(); load();
    };

    const op = document.createElement("select");
    op.setAttribute("aria-label", "filter operator");
    for (const name of field.operators) op.appendChild(new Option(operatorLabel(name), name));
    op.value = draft.op;
    op.onchange = () => { draft.op = op.value; draft.raw = ""; renderFilters(); reset(); load(); };

    const remove = el("button", "icon-button", "×");
    remove.title = "Remove filter";
    remove.onclick = () => { filters.splice(filters.indexOf(draft), 1); renderFilters(); reset(); load(); };
    row.append(fieldSelect, op, valueControl(draft, field), remove);
    host.appendChild(row);
  }
}

function builtFilter() {
  return {conditions: filters.map(draft => conditionFromDraft(findField(draft.field),
    draft.op, draft.raw))};
}

function tenantColumn(schema) {
  if (!session.demoEnabled) return null;
  const field = (schema.fields || []).find(field => !field.map && field.name === "TenantId");
  return field?.name || null;
}

function summarise(spans, binding, tenant) {
  const byTrace = new Map();
  for (const span of spans) {
    const id = span[binding.trace_id];
    let trace = byTrace.get(id);
    if (!trace) {
      trace = {trace_id: id, first: Infinity, last: -Infinity, spans: 0, root: "", tenant: ""};
      byTrace.set(id, trace);
    }
    const at = new Date(span[binding.timestamp]).getTime();
    trace.first = Math.min(trace.first, at);
    trace.last = Math.max(trace.last, at);
    trace.spans++;
    if (tenant && !trace.tenant) trace.tenant = span[tenant] || "";
    if (!span[binding.parent_span_id]) trace.root = span[binding.name] || trace.root;
    if (!trace.root && at === trace.first) trace.root = span[binding.name] || "";
  }
  return [...byTrace.values()];
}

export async function load() {
  if ($("view-list").hidden || loading || done) return;
  const generation = listGeneration;
  const controller = new AbortController();
  listController = controller;
  let timedOut = false;
  const watchdog = setTimeout(() => { timedOut = true; controller.abort(); }, requestTimeout);
  loading = true;
  $("sentinel").textContent = "loading…";
  $("load-more").hidden = !cursor;
  $("load-more").disabled = true;
  $("load-more").textContent = "Loading…";
  $("filter-error").hidden = true;
  try {
    const current = await ensureSchema(controller.signal, generation);
    if (!current || controller.signal.aborted || generation !== listGeneration) return;
    // A cursor continues one search. Keep relative presets and filter drafts from changing its
    // bounds or meaning between pages; Apply starts a new search through reset().
    if (!listSearch) listSearch = {window: windowParams(), filter: builtFilter()};
    const {window, filter} = listSearch;
    const tenant = tenantColumn(current);
    const columns = queryColumns(current);
    if (tenant) columns.push(tenant);
    const data = await api(API.traceQuery, {
      method: "POST",
      signal: controller.signal,
      preview: true,
      body: JSON.stringify({
        from: window.from, to: window.to, limit: 50,
        columns,
        ...(filter.conditions.length ? {filter} : {}),
        ...(cursor ? {cursor} : {}),
      }),
    });
    if (controller.signal.aborted || generation !== listGeneration) return;
    for (const trace of summarise(data.spans, data.binding, tenant))
      $("rows").appendChild(traceRow(trace));
    cursor = data.next_cursor || null;
    done = !cursor;
    retryDelay = 2000;
    $("sentinel").textContent = done
      ? ($("rows").children.length ? "end of results" : "no traces in this window")
      : "More traces are available.";
    $("load-more").hidden = done;
    $("load-more").disabled = false;
    $("load-more").textContent = "Load more";
  } catch (error) {
    if ((controller.signal.aborted && !timedOut) || generation !== listGeneration) return;
    if (error.message === "unauthenticated") {
      $("sentinel").textContent = "";
      return;
    }
    if (!error.status || transientStatuses.has(error.status)) {
      $("sentinel").textContent = "Trace source unavailable. Retrying automatically…";
      $("load-more").hidden = true;
      $("filter-error").hidden = true;
      scheduleRetry(generation);
      return;
    }
    const message = error.message;
    $("sentinel").textContent = "Could not load traces.";
    $("load-more").hidden = false;
    $("load-more").disabled = false;
    $("load-more").textContent = "Retry";
    if (message) { $("filter-error").textContent = message; $("filter-error").hidden = false; }
  } finally {
    clearTimeout(watchdog);
    if (generation === listGeneration) {
      loading = false;
      if (listController === controller) listController = null;
    }
  }
}

function traceRow(trace) {
  const row = document.createElement("tr");
  const root = cell("");
  const link = document.createElement("a");
  link.href = "#";
  link.textContent = trace.root || "unnamed trace";
  link.setAttribute("aria-label", `Open trace ${trace.trace_id}: ${trace.root || "unnamed trace"}`);
  link.onclick = event => {
    event.preventDefault();
    event.stopPropagation();
    return openTrace(trace.trace_id);
  };
  root.appendChild(link);
  const cells = [cell(new Date(trace.first).toLocaleString()), root];
  if (session.demoEnabled) cells.push(cell(trace.tenant || "—"));
  cells.push(cell(trace.spans, "num"), cell((trace.last - trace.first) + " ms", "num"),
    cell(trace.trace_id, "mono faint"));
  row.append(...cells);
  row.onclick = () => openTrace(trace.trace_id);
  return row;
}

function displayValue(value) {
  if (value && typeof value === "object") return JSON.stringify(value);
  return String(value ?? "");
}

function allFields(span) {
  const out = [];
  for (const [name, value] of Object.entries(span)) {
    if (value === undefined || value === null) continue;
    if (value && typeof value === "object" && !Array.isArray(value)) {
      const entries = Object.entries(value);
      if (entries.length) {
        for (const [key, nested] of entries)
          out.push([`${name}[${JSON.stringify(key)}]`, displayValue(nested)]);
        continue;
      }
    }
    out.push([name, displayValue(value)]);
  }
  return out;
}

function groupedFields(span, binding) {
  const structural = new Set(Object.values(binding));
  const groups = new Map([
    ["Overview", []], ["Input and output", []], ["Span attributes", []],
    ["Resource attributes", []], ["Other fields", []],
  ]);
  for (const field of allFields(span)) {
    const key = field[0];
    const lower = key.toLowerCase();
    let group = "Other fields";
    if (structural.has(key) || /(^|\.)(duration|status|kind|service\.name)$/i.test(key))
      group = "Overview";
    else if (/(input|output|prompt|completion|message|request|response)/.test(lower))
      group = "Input and output";
    else if (lower.startsWith("resourceattributes") || lower.startsWith("resource."))
      group = "Resource attributes";
    else if (lower.startsWith("spanattributes") || lower.startsWith("attributes") || lower.startsWith("span."))
      group = "Span attributes";
    groups.get(group).push(field);
  }
  return [...groups].filter(([, fields]) => fields.length);
}

async function openTrace(id) {
  detailController?.abort();
  const generation = ++detailGeneration;
  const controller = new AbortController();
  detailController = controller;
  $("detail-id").textContent = id;
  const host = $("spans");
  const status = el("p", "sentinel", "Loading trace…");
  status.setAttribute("role", "status");
  host.replaceChildren(status);
  $("winctl").hidden = true;
  $("view-list").hidden = true;
  $("view-detail").hidden = false;

  // Detail is a GET of an already-authorized trace. Omitting a projection asks the API for the
  // complete stored rows, including fields that were never marked as search filters.
  let data;
  try {
    data = await api(API.trace(id), {signal: controller.signal, preview: true});
  } catch (error) {
    if (controller.signal.aborted || generation !== detailGeneration || $("view-detail").hidden) return;
    status.className = "sentinel err mono";
    status.setAttribute("role", "alert");
    status.textContent = error.message === "unauthenticated" ? "" : `Could not load trace: ${error.message}`;
    return;
  } finally {
    if (generation === detailGeneration && detailController === controller) detailController = null;
  }
  if (controller.signal.aborted || generation !== detailGeneration || $("view-detail").hidden) return;
  host.replaceChildren();

  const binding = data.binding;
  const at = span => new Date(span[binding.timestamp]).getTime();
  let first = Infinity, last = -Infinity;
  for (const span of data.spans) {
    const time = at(span);
    first = Math.min(first, time);
    last = Math.max(last, time);
  }
  if (!data.spans.length) first = last = 0;
  const byID = new Map(data.spans.map(span => [span[binding.span_id], span]));
  const depths = new Map();
  const depth = (span, seen = new Set()) => {
    const id = span[binding.span_id];
    if (depths.has(id)) return depths.get(id);
    const parent = span[binding.parent_span_id];
    if (!parent || !byID.has(parent) || seen.has(id)) return 0;
    seen.add(id);
    const value = depth(byID.get(parent), seen) + 1;
    depths.set(id, value);
    return value;
  };

  for (const span of data.spans) {
    const row = document.createElement("details");
    row.className = "span";
    row.style.setProperty("--depth", depth(span));
    const line = el("div", "span-line");
    const tree = el("div", "span-tree");
    tree.appendChild(el("span", "span-dot"));
    const title = el("div", "span-title");
    title.appendChild(el("strong", "", span[binding.name] || "unnamed span"));
    title.appendChild(el("span", "span-meta mono",
      `+${at(span) - first}ms · ${span[binding.span_id] || "no span id"}`));
    tree.appendChild(title);
    const timeline = el("div", "timeline");
    const marker = el("i", "timeline-marker");
    marker.style.left = `${last === first ? 0 : ((at(span) - first) / (last - first)) * 100}%`;
    timeline.appendChild(marker);
    line.append(tree, timeline, el("span", "span-toggle", "›"));

    const fields = el("div", "span-fields");
    for (const [groupName, items] of groupedFields(span, binding)) {
      const group = el("section", "span-field-group");
      group.appendChild(el("h3", "", groupName));
      const attrs = el("div", "attrs");
      for (const [key, value] of items) {
        const item = el("div", "attr");
        item.append(el("span", "attr-key", key), el("span", "attr-value", value));
        attrs.appendChild(item);
      }
      group.appendChild(attrs);
      fields.appendChild(group);
    }
    if (!fields.children.length) fields.appendChild(el("span", "faint", "No fields"));
    const summary = el("summary", "span-summary");
    summary.appendChild(line);
    row.append(summary, fields);
    host.appendChild(row);
  }
}

$("filter-add").onclick = async () => {
  const current = await ensureSchema();
  if (!current) return;
  const field = filterableFields(current)[0];
  if (!field) return;
  filters.push({field: fieldId(field), op: field.operators[0], raw: ""});
  renderFilters();
};
$("back").onclick = () => {
  detailGeneration++;
  detailController?.abort();
  detailController = null;
  $("view-detail").hidden = true;
  $("view-list").hidden = false;
  $("winctl").hidden = false;
};
$("load-more").onclick = () => load();

// Leaving Traces invalidates both request streams. When the user comes back, resume an interrupted
// list page instead of letting the old response write into a view with different identity state.
for (const nav of document.querySelectorAll(".tab")) {
  nav.addEventListener("click", () => {
    if (nav.dataset.view === "traces") {
      queueMicrotask(() => { if (!$("view-list").hidden && !loading && !done) load(); });
      return;
    }
    listGeneration++;
    listController?.abort();
    listController = null;
    clearTimeout(retryTimer);
    retryTimer = null;
    retryDelay = 2000;
    loading = false;
    detailGeneration++;
    detailController?.abort();
    detailController = null;
  });
}
