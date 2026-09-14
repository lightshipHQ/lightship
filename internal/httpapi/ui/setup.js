import { $, API, BINDING_ROLES, api, el, setSetupProgress, viewErr } from "./core.js";
import { bindingColumns, logicalTypeForClickHouse } from "./trace-query.js";
import { dateRange, initDateRange } from "./date-range.js";

initDateRange("flds-range-preset", "flds-from", "flds-to", "flds-custom-range");

// ---- database setup: the guided first-run flow, and afterwards the schema editor ----------------
// This screen is a surface the admin acts on. Selecting fields writes through the same API an agent
// would use; nothing here relays JSON through a third party. When no table is bound it reads as a
// wizard; once bound, the same three cards stay fully editable — re-binding is allowed.

let disc = null; // last table-discovery response, cached per tab visit
let sampleGeneration = 0;

const F = { // fields screen state; the DOM renders it, the JS owns it
  rows: new Map(),           // key map\0name → {map,name,type,filterable,policy,origin}
  draft: null,
  dirty: false,              // unsaved edits survive a tab switch; a clean table reseeds
};

export const hasUnsavedFieldChanges = () => F.dirty;
window.addEventListener("beforeunload", event => {
  if (!F.dirty) return;
  event.preventDefault();
  event.returnValue = "";
});

const fkey = (map, name) => `${map}\x00${name}`;
const fref = f => f.map ? `${f.map}["${f.name}"]` : f.name;

export async function loadSchemaView() {
  viewErr("schema-err", null);
  $("schema-state").textContent = "Loading…";
  disc = null; // rediscover on each tab entry; the source database is not ours and can change
  try {
    const d = await api(API.schema);
    const m = d.model;
    setSetupProgress(m);
    const state = !m.binding.table ? "Setup required" : d.compiles ? "Ready" : "Needs attention";
    $("schema-state").textContent = state;
    $("schema-state").title = `Configuration version ${m.version}`;
    $("schema-compile-err").hidden = d.compiles;
    $("schema-compile-err").textContent = d.compiles ? "" : d.error;

    if (m.binding.table) {
      renderBindGrid(m.binding);
      $("src-current").hidden = false;
      $("src-pick").hidden = true;
      await unlockFields(m);
    } else {
      $("src-current").hidden = true;
      $("flds-locked").hidden = false;
      $("flds-body").hidden = true;
      openPicker(false);
    }
  } catch (e) { $("schema-state").textContent = "Unavailable"; viewErr("schema-err", e); }
}

function renderBindGrid(b) {
  const grid = $("src-bindgrid");
  grid.replaceChildren();
  grid.appendChild(el("div", "mono source-table", b.table));
  const fields = [["Trace ID", b.trace_id], ["Timestamp", b.timestamp], ["Span ID", b.span_id],
    ["Parent", b.parent_span_id], ["Name", b.name]];
  grid.appendChild(el("p", "mono source-fields",
    fields.map(([label, value]) => `${label}: ${value}`).join(" · ")));
}

// ---- step 1: source ----------------------------------------------------------------------------

let pickerCancellable = false;

async function openPicker(cancellable) {
  pickerCancellable = cancellable;
  $("src-pick").hidden = false;
  $("src-editor").hidden = true;
  $("src-cancelrow").hidden = !cancellable;
  $("src-status").textContent = "Finding trace tables…";
  $("src-cands").replaceChildren();
  if (!disc) {
    try {
      disc = await api(API.discover, { method: "POST", body: "{}" });
    } catch (e) {
      $("src-status").textContent = "";
      $("src-status").appendChild(el("span", "err mono", "discovery failed: " + e.message));
      return;
    }
  }
  const cands = disc.tables || [];
  $("src-status").textContent = cands.length
    ? `${cands.length} likely trace table${cands.length === 1 ? "" : "s"}, best match first. Review before choosing.`
    : "No trace tables found.";
  for (const c of cands) $("src-cands").appendChild(candRow(c));
}

function candRow(c) {
  const roles = BINDING_ROLES;
  const div = el("div", "cand");
  const head = el("div", "row");
  head.appendChild(el("b", "mono", `${c.database}.${c.table}`));
  head.appendChild(el("span", "badge", `${c.score}/5 columns matched`));
  head.appendChild(el("span", "spacer"));
  const pick = el("button", "", "Select");
  pick.onclick = () => openBindEditor(c);
  head.appendChild(pick);
  div.appendChild(head);

  // Enough column evidence to justify the rank, without dumping every column of every table.
  const matched = roles.filter(r => c.suggested_binding[r]);
  const missing = roles.filter(r => !c.suggested_binding[r]);
  const ev = el("div", "muted mono", matched.map(r => `${r}→${c.suggested_binding[r]}`).join(" · ")
    || "no role columns recognised");
  ev.style.cssText = "font-size:12px;margin-top:4px";
  div.appendChild(ev);
  if (missing.length)
    div.appendChild(el("div", "err mono",
      "no match for: " + missing.join(", "))).style.cssText = "font-size:12px;margin-top:2px";
  const maps = c.columns.filter(x => x.type.startsWith("Map(")).map(x => x.name);
  div.appendChild(el("div", "faint", `${c.columns.length} columns` +
    (maps.length ? ` · map columns: ${maps.join(", ")}` : " · no map columns")))
    .style.cssText = "font-size:12px;margin-top:2px";
  return div;
}

let editCand = null;

function openBindEditor(c) {
  editCand = c;
  $("src-editor").hidden = false;
  $("src-cands").replaceChildren();
  $("src-status").textContent = "";
  $("src-err").hidden = true;
  $("src-table").textContent = `${c.database}.${c.table}`;
  const form = $("src-roles");
  form.replaceChildren();
  for (const r of BINDING_ROLES) {
    form.appendChild(el("label", "faint mono", r.replace(/_/g, " ")));
    const sel = document.createElement("select");
    sel.id = "role-" + r;
    sel.appendChild(new Option("— choose a column —", ""));
    for (const col of c.columns) sel.appendChild(new Option(`${col.name}  (${col.type})`, col.name));
    sel.value = c.suggested_binding[r] || "";
    form.appendChild(sel);
  }
}

$("src-editor-back").onclick = () => openPicker(pickerCancellable);
$("src-change").onclick = () => { $("src-current").hidden = true; openPicker(true); };
$("src-cancel").onclick = () => loadSchemaView();

$("src-confirm").onclick = async () => {
  $("src-err").hidden = true;
  const body = { table: `${editCand.database}.${editCand.table}` };
  for (const r of BINDING_ROLES)
    body[r] = $("role-" + r).value;
  try {
    await api(API.binding, { method: "PUT", body: JSON.stringify(body) });
  } catch (e) {
    // The server's error, verbatim — it names each missing role.
    $("src-err").hidden = false;
    $("src-err").textContent = e.message;
    return;
  }
  F.dirty = false; // a new table invalidates any pending field edits
  await loadSchemaView();
};

// ---- step 2: fields ----------------------------------------------------------------------------

const typeLabels = { string: "Text", string_array: "List", boolean: "True / false", number: "Number" };

function mapValueType(raw) {
  const open = raw.indexOf("("), close = raw.lastIndexOf(")");
  if (open < 0 || close <= open) return "String";
  const inner = raw.slice(open + 1, close);
  let depth = 0;
  for (let i = 0; i < inner.length; i++) {
    if (inner[i] === "(") depth++;
    else if (inner[i] === ")") depth--;
    else if (inner[i] === "," && depth === 0) return inner.slice(i + 1).trim();
  }
  return "String";
}

function inferredType(map, name) {
  if (map) return F.mapTypes.get(map) || "string";
  return logicalTypeForClickHouse(F.columnTypes.get(name));
}

async function unlockFields(m) {
  $("flds-locked").hidden = true;
  $("flds-body").hidden = false;
  if (F.dirty && F.boundTable === m.binding.table) { renderFields(); return; }
  F.boundTable = m.binding.table;
  F.binding = m.binding;

  // The candidate listing supplies physical types for read-only display. Map keys are discovered
  // from recent data because ClickHouse exposes the map column, not the keys stored inside it.
  if (!disc) {
    try { disc = await api(API.discover, { method: "POST", body: "{}" }); }
    catch (e) { disc = { tables: [] }; }
  }
  const cand = (disc.tables || []).find(c => `${c.database}.${c.table}` === m.binding.table);

  const structural = new Set(bindingColumns(m.binding));

  // Seed from the stored model first: the saved set REPLACES wholesale, so every marked field must
  // be present and checked before save can be correct — including ones the sample won't return.
  F.rows.clear();
  F.draft = null;
  for (const f of m.fields || [])
    F.rows.set(fkey(f.map, f.name), { map: f.map, name: f.name, filterable: f.filterable,
      policy: f.policy, type: f.logical_type || "string", origin: "marked",
      bound: !f.map && structural.has(f.name) });

  F.columnTypes = new Map();
  F.mapTypes = new Map();
  if (cand) {
    for (const col of cand.columns) {
      F.columnTypes.set(col.name, col.type);
      if (col.type.startsWith("Map(")) {
        F.mapTypes.set(col.name, logicalTypeForClickHouse(mapValueType(col.type)));
        continue;
      }
      if (!col.type.includes("String") && !structural.has(col.name)) continue;
      const k = fkey("", col.name);
      if (!F.rows.has(k)) F.rows.set(k, { map: "", name: col.name, filterable: false,
        policy: false, type: logicalTypeForClickHouse(col.type),
        origin: structural.has(col.name) ? "binding" : "column", bound: structural.has(col.name) });
    }
    // Discovery pre-offers columns worth marking (ServiceName today); precheck only what is not
    // already part of the stored set, so a saved decision is never overridden.
    for (const f of cand.suggested_fields || []) {
      const row = F.rows.get(fkey(f.map || "", f.name));
      if (row && row.origin === "column" && f.filterable) row.filterable = true;
    }
  }
  // A bound column remains configurable even if discovery is temporarily unavailable or no
  // longer returns it. Binding defines its structural role; the field flags define query access.
  for (const name of structural) {
    const k = fkey("", name);
    if (!F.rows.has(k)) F.rows.set(k, { map: "", name, filterable: false, policy: false,
      type: inferredType("", name), origin: "binding", bound: true });
  }
  $("flds-discovery").hidden = F.mapTypes.size === 0;
  if (!F.mapTypes.size) $("flds-samplenote").textContent = "";

  renderFields();
  if (F.mapTypes.size) await sampleKeys(); // fresh keys on every entry; saved choices survive it
}

async function sampleKeys() {
  const generation = ++sampleGeneration;
  const maps = [...F.mapTypes.keys()];
  $("flds-err").hidden = true;
  $("flds-err").className = "err mono";
  if (!maps.length) return;
  $("flds-samplenote").textContent = "Refreshing fields…";
  $("flds-sample").disabled = true;
  let d;
  try {
    const range = dateRange("flds-from", "flds-to", "flds-range-preset");
    d = await api(API.discover, { method: "POST", body: JSON.stringify({
      table: F.binding.table, timestamp: F.binding.timestamp,
      maps, from: range.from, to: range.to }) });
  } catch (e) {
    if (generation !== sampleGeneration) return;
    $("flds-samplenote").textContent = "";
    $("flds-err").hidden = false;
    $("flds-err").textContent = "sampling failed: " + e.message;
    $("flds-sample").disabled = false;
    return;
  }
  if (generation !== sampleGeneration) return;
  // Refresh unchecked discovered keys while keeping saved and newly added fields intact.
  for (const [k, f] of [...F.rows]) {
    if (f.origin === "sample" && !f.filterable && !f.policy) F.rows.delete(k);
  }
  for (const kk of d.keys || []) {
    const k = fkey(kk.map, kk.key);
    let f = F.rows.get(k);
    if (!f) { f = { map: kk.map, key: kk.key, name: kk.key, filterable: false, policy: false,
      type: F.mapTypes.get(kk.map) || "string", origin: "sample" }; F.rows.set(k, f); }
  }
  $("flds-samplenote").textContent = `${(d.keys || []).length} fields found in the selected period.`;
  $("flds-sample").disabled = false;
  renderFields();
}
$("flds-sample").onclick = sampleKeys;

function permissionCell(f, flag) {
  const td = document.createElement("td");
  td.className = "ck";
  const cb = document.createElement("input");
  cb.type = "checkbox";
  cb.checked = f[flag];
  cb.setAttribute("aria-label", `${flag === "filterable" ? "Searchable" : "Access rules"}: ${f.name || "new field"}`);
  if (flag === "policy" && f.type === "number") {
    cb.disabled = true;
    cb.title = "Number fields cannot be used in access rules";
  }
  cb.onchange = () => { f[flag] = cb.checked; F.dirty = true; updateSummary(); };
  td.appendChild(cb);
  return td;
}

function fieldRow(f, duplicateName) {
  const tr = document.createElement("tr");
  tr.dataset.key = fkey(f.map, f.name);
  tr.dataset.hay = fref(f).toLowerCase();

  const nameCell = document.createElement("td");
  nameCell.appendChild(el("span", "field-name", f.name));
  if (duplicateName && f.map) nameCell.appendChild(el("span", "field-source", f.map));
  nameCell.title = fref(f);
  const typeCell = document.createElement("td");
  typeCell.appendChild(el("span", "field-type", typeLabels[f.type] || typeLabels.string));
  const action = document.createElement("td");
  if (f.bound) {
    action.appendChild(el("span", "faint", "bound"));
  } else {
    const remove = el("button", "field-remove", "×");
    remove.title = `Remove ${f.name}`;
    remove.setAttribute("aria-label", `Remove ${f.name}`);
    remove.onclick = () => { F.rows.delete(fkey(f.map, f.name)); F.dirty = true; renderFields(); };
    action.appendChild(remove);
  }
  tr.append(nameCell, typeCell, permissionCell(f, "filterable"), permissionCell(f, "policy"), action);
  return tr;
}

function draftRow(f) {
  const tr = document.createElement("tr");
  tr.dataset.draft = "true";
  const fieldCell = document.createElement("td");
  const editor = el("div", "field-editor");
  const source = document.createElement("select");
  source.setAttribute("aria-label", "Field source");
  source.appendChild(new Option("Table column", ""));
  for (const map of F.mapTypes.keys()) source.appendChild(new Option(map, map));
  source.hidden = F.mapTypes.size === 0;
  source.value = f.map;
  const input = document.createElement("input");
  input.placeholder = f.map ? "Attribute key" : "Column name";
  input.setAttribute("aria-label", "Field name");
  input.value = f.name;
  const typeCell = document.createElement("td");
  const searchableCell = permissionCell(f, "filterable");
  const policyCell = permissionCell(f, "policy");
  const renderType = () => {
    f.type = inferredType(f.map, f.name.trim());
    if (f.type === "number") f.policy = false;
    typeCell.replaceChildren(el("span", "field-type", typeLabels[f.type] || typeLabels.string));
    const policy = policyCell.querySelector("input");
    policy.checked = f.policy;
    policy.disabled = f.type === "number";
    policy.title = f.type === "number" ? "Number fields cannot be used in access rules" : "";
    input.placeholder = f.map ? "Attribute key" : "Column name";
    tr.dataset.hay = `${f.map} ${f.name}`.toLowerCase();
  };
  source.onchange = () => { f.map = source.value; renderType(); renderFields(); };
  input.oninput = () => { f.name = input.value; renderType(); F.dirty = true; updateSummary(); };
  input.onkeydown = event => { if (event.key === "Escape") { F.draft = null; renderFields(); } };
  editor.append(source, input); fieldCell.appendChild(editor);
  const action = document.createElement("td");
  const remove = el("button", "field-remove", "×");
  remove.title = "Cancel new field";
  remove.setAttribute("aria-label", "Cancel new field");
  remove.onclick = () => { F.draft = null; renderFields(); };
  action.appendChild(remove);
  renderType();
  tr.append(fieldCell, typeCell, searchableCell, policyCell, action);
  return tr;
}

function renderFields() {
  const rows = [...F.rows.values()].sort((a, b) => fref(a).localeCompare(fref(b)));
  const counts = new Map();
  for (const f of rows) counts.set(f.name, (counts.get(f.name) || 0) + 1);
  const frag = document.createDocumentFragment();
  for (const f of rows) frag.appendChild(fieldRow(f, counts.get(f.name) > 1));
  if (F.draft) frag.appendChild(draftRow(F.draft));
  $("flds-rows").replaceChildren(frag);
  applySearch();
  updateSummary();
}

// Search hides rows in place rather than re-rendering: 220 rows toggle instantly and no checkbox
// is ever rebuilt while someone is clicking it.
function applySearch() {
  const q = $("flds-q").value.toLowerCase();
  for (const tr of $("flds-rows").children)
    tr.hidden = tr.dataset.draft !== "true" && q && !tr.dataset.hay.includes(q);
}
$("flds-q").oninput = applySearch;

$("flds-add").onclick = () => {
  if (!F.draft) F.draft = { map: "", name: "", type: "string", filterable: true,
    policy: false, origin: "manual" };
  renderFields();
  $("flds-rows").querySelector("tr[data-draft] input")?.focus();
};

function markedFields() {
  const rows = [...F.rows.values()];
  if (F.draft?.name.trim()) rows.push({ ...F.draft, name: F.draft.name.trim() });
  return rows.filter(f => f.filterable || f.policy)
    .map(f => ({ map: f.map, name: f.name, logical_type: f.type || "string",
      filterable: f.filterable, policy: f.policy }));
}

function updateSummary() {
  const count = markedFields().length;
  $("flds-summary").textContent = `${count} field${count === 1 ? "" : "s"} selected` +
    (F.dirty ? " · Unsaved changes" : "");
}

$("flds-save").onclick = async () => {
  $("flds-err").hidden = true;
  $("flds-err").className = "err mono";
  for (const tr of $("flds-rows").children) tr.classList.remove("rowerr");
  const body = { fields: markedFields() };
  const keys = new Set();
  for (const field of body.fields) {
    const key = fkey(field.map, field.name);
    if (keys.has(key)) {
      $("flds-err").hidden = false;
      $("flds-err").textContent = `${fref(field)} is already in the list.`;
      return;
    }
    keys.add(key);
  }
  $("flds-save").disabled = true;
  try {
    await api(API.fields, { method: "PUT", body: JSON.stringify(body) });
  } catch (e) {
    // Verbatim: a compile rejection means a policy still names an unmarked field — the role must
    // be amended first, via your agent. Highlight any row the message names.
    $("flds-err").hidden = false;
    $("flds-err").textContent = e.message;
    for (const tr of $("flds-rows").children) {
      const f = F.rows.get(tr.dataset.key);
      if (f && e.message.includes(fref(f))) tr.classList.add("rowerr");
    }
    $("flds-save").disabled = false;
    return;
  }
  F.dirty = false;
  if (F.draft?.name.trim() && (F.draft.filterable || F.draft.policy)) {
    const added = { ...F.draft, name: F.draft.name.trim() };
    F.rows.set(fkey(added.map, added.name), added);
  }
  F.draft = null;
  renderFields();
  const d = await api(API.schema).catch(() => null);
  if (d) {
    setSetupProgress(d.model);
    $("schema-state").textContent = "Ready";
    $("schema-state").title = `Configuration version ${d.model.version}`;
  }
  $("flds-err").hidden = false;
  $("flds-err").className = "muted";
  $("flds-err").textContent = `Saved ${body.fields.length} field${body.fields.length === 1 ? "" : "s"}.`;
  $("flds-save").disabled = false;
  setTimeout(() => { if (!F.dirty) { $("flds-err").hidden = true; $("flds-err").className = "err mono"; } }, 6000);
};
