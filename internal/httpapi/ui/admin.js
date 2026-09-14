import { $, API, api, cell, el, setSetupProgress, viewErr } from "./core.js";
import { buildRolePolicy, parseRolePolicies, readablePolicy, roleFields, roleOperators } from "./role-builder.js";

let roleAttrs = [];
let roleModel = null;
let roleDraft = null;
const option = (value, label, selected = false) => {
  const item = document.createElement("option");
  item.value = value;
  item.textContent = label;
  item.selected = selected;
  return item;
};

function userRolePicker(availableRoles, assignedRoles = [], label = "Roles") {
  const picker = el("div", "role-picker");
  picker.setAttribute("role", "group");
  picker.setAttribute("aria-label", label);
  picker.roleInputs = [];
  const names = [...new Set([...availableRoles, ...assignedRoles])];
  const pinned = names.filter(name => assignedRoles.includes(name));
  const visible = [...pinned];
  for (const name of names) {
    if (visible.length >= 3) break;
    if (!visible.includes(name)) visible.push(name);
  }
  const overflow = names.filter(name => !visible.includes(name));
  const appendRole = (host, name) => {
    const label = el("label", "role-choice");
    const input = document.createElement("input");
    input.type = "checkbox";
    input.value = name;
    input.checked = assignedRoles.includes(name);
    input.setAttribute("aria-label", `Role ${name}`);
    label.append(input, el("span", "", name + (availableRoles.includes(name) ? "" : " (unavailable)")));
    picker.roleInputs.push(input);
    host.appendChild(label);
  };
  for (const name of visible) appendRole(picker, name);
  if (overflow.length) {
    const more = document.createElement("details");
    more.className = "role-picker-more";
    more.appendChild(el("summary", "", `${overflow.length} more`));
    const options = el("div", "role-picker-more-options");
    for (const name of overflow) appendRole(options, name);
    more.appendChild(options);
    picker.appendChild(more);
  }
  if (!names.length) picker.appendChild(el("span", "faint", "No roles available"));
  return picker;
}

const selectedRoles = picker => picker.roleInputs.filter(input => input.checked)
  .map(input => input.value);

function defaultCondition() {
  const field = roleFields(roleModel || {})[0];
  return field ? { field: field.ref, op: roleOperators(field.type)[0].value,
    source: "literal", value: field.type === "boolean" ? "true" : "" } : null;
}

function closeRoleBuilder() {
  roleDraft = null;
  renderRoles();
}

async function deleteRole(name, message) {
  if (!confirm(`Delete role ${name} and remove all its assignments?`)) return;
  try {
    await api(API.roles(name), { method: "DELETE" });
    await loadRoles();
  } catch (error) { message.textContent = error.message; }
}

function renderAdvancedRoleBuilder(host) {
  const message = el("p", "muted",
    "This role uses an advanced policy that the visual builder cannot safely change.");
  const docs = document.createElement("a");
  docs.href = "https://lightship.mintlifysite.com/docs/concepts/policies";
  docs.target = "_blank";
  docs.rel = "noreferrer";
  docs.textContent = "Review the policy documentation";
  message.append(" ", docs, ".");
  host.appendChild(message);
}

function renderCondition(condition, index, fields) {
  const row = el("div", "condition-row");
  const field = fields.find(item => item.ref === condition.field) || fields[0];

  const fieldSelect = document.createElement("select");
  fieldSelect.setAttribute("aria-label", "Trace field");
  for (const item of fields) fieldSelect.appendChild(option(item.ref, item.ref, item.ref === field.ref));
  fieldSelect.onchange = () => {
    const next = fields.find(item => item.ref === fieldSelect.value);
    roleDraft.conditions[index] = { field: next.ref, op: roleOperators(next.type)[0].value,
      source: "literal", value: next.type === "boolean" ? "true" : "" };
    renderRoles();
  };

  const operatorSelect = document.createElement("select");
  operatorSelect.setAttribute("aria-label", "Operator");
  for (const operator of roleOperators(field.type))
    operatorSelect.appendChild(option(operator.value, operator.label, operator.value === condition.op));
  operatorSelect.onchange = () => { condition.op = operatorSelect.value; };

  const sourceSelect = document.createElement("select");
  sourceSelect.setAttribute("aria-label", "Value source");
  if (field.type === "boolean") {
    sourceSelect.appendChild(option("literal", "True or false", true));
    sourceSelect.disabled = true;
  } else {
    sourceSelect.appendChild(option("literal", "Custom value", condition.source !== "user"));
    if (field.type === "string" && roleAttrs.length)
      sourceSelect.appendChild(option("user", "User attribute", condition.source === "user"));
  }
  sourceSelect.onchange = () => {
    condition.source = sourceSelect.value;
    condition.value = condition.source === "user" ? roleAttrs[0] || "" : "";
    renderRoles();
  };

  let valueControl;
  if (field.type === "boolean") {
    valueControl = document.createElement("select");
    valueControl.append(option("true", "True", condition.value !== "false"),
      option("false", "False", condition.value === "false"));
  } else if (condition.source === "user") {
    valueControl = document.createElement("select");
    for (const attr of roleAttrs)
      valueControl.appendChild(option(attr, `user.${attr}`, attr === condition.value));
  } else {
    valueControl = document.createElement("input");
    valueControl.placeholder = field.type === "string_array" ? "Value in the array" : "Enter a value";
    valueControl.value = condition.value;
  }
  valueControl.setAttribute("aria-label", "Condition value");
  valueControl.onchange = () => { condition.value = valueControl.value; };
  valueControl.oninput = () => { condition.value = valueControl.value; };

  const remove = el("button", "field-remove", "×");
  remove.title = "Remove condition";
  remove.setAttribute("aria-label", "Remove condition");
  remove.onclick = () => { roleDraft.conditions.splice(index, 1); renderRoles(); };
  row.append(fieldSelect, operatorSelect, sourceSelect, valueControl, remove);
  return row;
}

function roleBuilderCard() {
  const host = el("div", "card role-builder");

  const head = el("div", "role-builder-head");
  head.appendChild(el("h2", "", roleDraft.isNew ? "Create a role" : `Edit ${roleDraft.name}`));
  host.appendChild(head);

  const message = el("p", "err", "");
  if (roleDraft.advanced) {
    renderAdvancedRoleBuilder(host);
  } else {
    if (roleDraft.isNew) {
      const label = el("label", "", "Role name");
      label.setAttribute("for", "role-builder-name");
      const name = document.createElement("input");
      name.id = "role-builder-name";
      name.className = "role-builder-name";
      name.placeholder = "For example, support";
      name.value = roleDraft.name;
      name.oninput = () => { roleDraft.name = name.value; };
      host.append(label, name);
    }

    const fields = roleFields(roleModel || {});
    if (!fields.length) {
      host.appendChild(el("p", "notice", "Choose at least one access-rule field in Connect database before creating a role."));
    } else {
      const match = el("div", "role-match");
      match.appendChild(el("label", "", "A trace is visible when"));
      const matchSelect = document.createElement("select");
      matchSelect.append(option("all", "all conditions match", roleDraft.match === "all"),
        option("any", "any condition matches", roleDraft.match === "any"));
      matchSelect.onchange = () => { roleDraft.match = matchSelect.value; };
      match.appendChild(matchSelect);
      host.appendChild(match);
      roleDraft.conditions.forEach((condition, index) =>
        host.appendChild(renderCondition(condition, index, fields)));
      const add = el("button", "", "+ Add condition");
      add.style.marginTop = "10px";
      add.onclick = () => { roleDraft.conditions.push(defaultCondition()); renderRoles(); };
      host.appendChild(add);
    }
  }

  const actions = el("div", "builder-actions");
  if (!roleDraft.advanced) {
    const save = el("button", "primary", roleDraft.isNew ? "Create role" : "Save changes");
    save.disabled = !roleFields(roleModel || {}).length;
    save.onclick = async () => {
      message.textContent = "";
      const name = roleDraft.name.trim();
      if (!name) { message.textContent = "Enter a role name."; return; }
      if (roleDraft.isNew && (roleModel.roles || []).some(role => role.name === name)) {
        message.textContent = "A role with this name already exists.";
        return;
      }
      try {
        const expression = buildRolePolicy(roleDraft.conditions, roleFields(roleModel), roleDraft.match);
        save.disabled = true;
        await api(API.roles(name), { method: "PUT", body: JSON.stringify({ policies: [{
          title: name, description: readablePolicy(expression), expression,
        }] }) });
        await loadRoles();
      } catch (error) { message.textContent = error.message; save.disabled = false; }
    };
    actions.appendChild(save);
  }
  const cancel = el("button", "", "Cancel");
  cancel.onclick = closeRoleBuilder;
  actions.appendChild(cancel);
  if (!roleDraft.isNew) {
    const remove = el("button", "delete-role", "Delete role");
    remove.onclick = () => deleteRole(roleDraft.name, message);
    actions.appendChild(remove);
  }
  host.append(actions, message);
  return host;
}

function editRole(role) {
  const parsed = parseRolePolicies(role.policies, roleFields(roleModel), roleAttrs);
  roleDraft = parsed
    ? { name: role.name, isNew: false, match: parsed.match, conditions: parsed.conditions }
    : { name: role.name, isNew: false, advanced: true, conditions: [] };
  renderRoles();
}

function renderRoleCard(role) {
  const card = el("div", "card role-card");
  const head = el("div", "role-card-head");
  const title = el("div");
  title.appendChild(el("h2", "", role.name));
  if (role.name === "admin") title.appendChild(el("span", "badge", "Built in"));
  head.appendChild(title);
  if (role.name !== "admin") {
    const edit = el("button", "", "Edit");
    edit.onclick = () => editRole(role);
    head.appendChild(edit);
  }
  card.appendChild(head);
  const policies = role.policies || [];
  if (policies.length) {
    for (const policy of policies) {
      const summary = el("div", "role-policy");
      summary.appendChild(el("p", "role-summary", role.name === "admin"
        ? "All traces"
        : readablePolicy(policy.expression || "")));
      const config = document.createElement("details");
      config.className = "policy-config";
      config.appendChild(el("summary", "", "View policy configuration"));
      config.appendChild(el("pre", "expr", policy.expression || ""));
      summary.appendChild(config);
      card.appendChild(summary);
    }
  } else card.appendChild(el("p", "muted", "No trace access configured."));
  return card;
}

function renderRoles() {
  const host = $("roles-list");
  host.replaceChildren();
  if (roleDraft?.isNew) host.appendChild(roleBuilderCard());
  for (const role of roleModel?.roles || []) {
    host.appendChild(!roleDraft?.isNew && roleDraft?.name === role.name
      ? roleBuilderCard()
      : renderRoleCard(role));
  }
}

export async function loadRoles() {
  viewErr("roles-err", null);
  $("roles-list").replaceChildren(el("p", "sentinel", "Loading roles…"));
  try {
    const d = await api(API.schema);
    roleModel = d.model;
    setSetupProgress(d.model);
    roleAttrs = (d.model.user_attributes || []).slice();
    roleDraft = null;
    renderRoles();
  } catch (e) { $("roles-list").replaceChildren(); viewErr("roles-err", e); }
}

$("role-new").onclick = () => {
  viewErr("roles-err", null);
  const condition = defaultCondition();
  roleDraft = { name: "", isNew: true, match: "all", conditions: condition ? [condition] : [] };
  renderRoles();
};

export async function loadUsers() {
  viewErr("users-err", null);
  $("users-note").textContent = "Loading users…";
  $("users-rows").replaceChildren();
  try {
    const [d, schema] = await Promise.all([api(API.users), api(API.schema)]);
    const availableRoles = (schema.model?.roles || []).map(role => role.name);
    const newRoles = $("user-new-roles");
    const selectedNewRoles = newRoles.roleInputs ? selectedRoles(newRoles) : [];
    const freshNewRoles = userRolePicker(availableRoles,
      selectedNewRoles.filter(name => availableRoles.includes(name)));
    newRoles.replaceChildren(...freshNewRoles.children);
    newRoles.roleInputs = freshNewRoles.roleInputs;

    const host = $("users-rows");
    host.replaceChildren();
    for (const u of d.users || []) {
      const tr = document.createElement("tr");
      tr.appendChild(cell(u.username, "mono"));
      const roles = document.createElement("td");
      const ri = userRolePicker(availableRoles, u.roles || [], `Roles for ${u.username}`);
      for (const input of ri.roleInputs) input.disabled = u.username === "admin";
      roles.appendChild(ri);
      const actions = document.createElement("td");
      if (u.username !== "admin") {
        const save = el("button", "", "Save"); actions.appendChild(save);
        save.onclick = async () => {
          save.disabled = true;
          viewErr("users-err", null);
          try {
            await api(API.user(u.username), {method:"PATCH",
              body:JSON.stringify({roles:selectedRoles(ri)})});
            if (!await loadUsers()) viewErr("users-err",
              new Error("The role was saved, but refreshing users failed. Reload to see the current state."));
          } catch (e) {
            viewErr("users-err", new Error(`Could not save roles for ${u.username}: ${e.message}`));
          } finally { save.disabled = false; }
        };
      } else actions.appendChild(el("span", "faint", "protected"));
      tr.append(roles, actions);
      host.appendChild(tr);
    }
    $("users-note").textContent = host.children.length ? "" : "No users yet.";
    return true;
  } catch (e) { $("users-note").textContent = ""; viewErr("users-err", e); return false; }
}

$("user-new").onclick = async () => {
  viewErr("users-err", null); $("user-password").hidden = true;
  $("user-new").disabled = true;
  try {
    const d = await api(API.users, {method:"POST", body:JSON.stringify({
      username:$("user-new-name").value.trim(), roles:selectedRoles($("user-new-roles")),
    })});
    $("user-password").replaceChildren(
      el("b", "", `Temporary password for ${d.username}`),
      el("div", "t", d.password),
      el("span", "muted", "Copy it now. It is shown once and must be changed at first sign-in."),
    );
    $("user-password").hidden = false;
    $("user-new-name").value = "";
    for (const input of $("user-new-roles").roleInputs) input.checked = false;
    await loadUsers();
  } catch (e) {
    viewErr("users-err", new Error("Could not create the user: " + e.message));
  } finally { $("user-new").disabled = false; }
};

// The audit endpoint takes a limit and returns newest-first; there is no cursor, so this is the
// most recent slice, filtered client-side, and it says so when the slice is full.
const auditLimit = 500;
let auditEntries = [];

export async function loadAudit() {
  viewErr("audit-err", null);
  $("audit-note").textContent = "Loading audit entries…";
  $("audit-rows").replaceChildren();
  try {
    const d = await api(API.audit(auditLimit));
    auditEntries = d.entries || [];
    $("audit-note").textContent = auditEntries.length >= auditLimit
      ? `showing the most recent ${auditLimit} entries`
      : (auditEntries.length ? "end of log" : "no audit entries");
    renderAudit();
  } catch (e) { viewErr("audit-err", e); }
}

function renderAudit() {
  const q = $("audit-q").value.toLowerCase();
  const host = $("audit-rows");
  host.replaceChildren();
  for (const en of auditEntries) {
    const via = (en.detail || {}).via_key || "";
    const parts = [];
    for (const [k, v] of Object.entries(en.detail || {})) {
      if (k === "via_key") continue;
      parts.push(`${k}=${typeof v === "string" ? v : JSON.stringify(v)}`);
    }
    if (en.policy_filters) parts.push(`filters=${en.policy_filters}`);
    const hay = `${en.username} ${en.action} ${via} ${parts.join(" ")}`.toLowerCase();
    if (q && !hay.includes(q)) continue;

    const tr = document.createElement("tr");
    tr.appendChild(cell(new Date(en.at).toLocaleString()));
    const actor = document.createElement("td");
    actor.appendChild(el("span", "mono", en.username));
    if (via) actor.appendChild(el("span", "badge", "via key " + via));
    tr.append(actor, cell(en.action, "mono"), cell(parts.join("  "), "mono faint"));
    host.appendChild(tr);
  }
  if (!host.children.length && auditEntries.length)
    $("audit-note").textContent = "no entries match the filter";
}
$("audit-q").oninput = renderAudit;
