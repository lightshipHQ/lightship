// Everything here goes through the same JSON API an API-only deployment exposes. The browser has
// no query path of its own, so a policy filter cannot be missing from one and present in the other.
export const $ = id => document.getElementById(id);
export const session = {
  username: "", isAdmin: false, actorIsAdmin: false, isDemo: false, demoEnabled: false,
  passwordForced: false, previewRole: "", demoWindow: null,
};
let bootHandler = async () => {};

// Shared API and schema vocabulary. UI modules use these names rather than carrying parallel
// endpoint strings or structural-role lists that can drift independently.
export const API = Object.freeze({
  login: "/login",
  demo: "/demo",
  logout: "/logout",
  me: "/me",
  password: "/me/password",
  traceQuery: "/traces/query",
  trace: id => "/traces/" + encodeURIComponent(id),
  filterSchema: "/filter/schema",
  schema: "/schema",
  discover: "/schema/discover",
  binding: "/schema/binding",
  fields: "/schema/fields",
  keys: "/keys",
  key: id => "/keys/" + encodeURIComponent(id),
  users: "/users",
  user: name => "/users/" + encodeURIComponent(name),
  userAttributes: name => "/users/" + encodeURIComponent(name) + "/attributes",
  roles: name => "/roles/" + encodeURIComponent(name),
  audit: limit => "/audit?limit=" + limit,
});
export const BINDING_ROLES = Object.freeze([
  "trace_id", "timestamp", "span_id", "parent_span_id", "name",
]);

export function setSetupProgress(model) {
  if (!model) return;
  const connected = !!model.binding?.table;
  const fieldsReady = connected && (model.fields || []).some(field => field.policy);
  const roleReady = fieldsReady && (model.roles || []).some(role => role.name !== "admin");
  const roles = document.querySelector('.subnav .tab[data-view="roles"]');
  const users = document.querySelector('.subnav .tab[data-view="users"]');
  if (roles) {
    roles.disabled = !fieldsReady;
    roles.title = fieldsReady ? "" : "Choose at least one access-rules field first";
    roles.setAttribute("aria-label", fieldsReady ? "Define roles" :
      "Define roles, unavailable until an access-rules field is selected");
  }
  if (users) {
    users.disabled = !roleReady;
    users.title = roleReady ? "" : "Create an access role first";
    users.setAttribute("aria-label", roleReady ? "Manage users" :
      "Manage users, unavailable until an access role is created");
  }
}

export async function api(path, opts) {
  const { preview = false, ...requestOptions } = opts || {};
  const options = {...requestOptions};
  options.headers = {...(requestOptions.headers || {})};
  if (preview && session.previewRole)
    options.headers["X-LightShip-Preview-Role"] = session.previewRole;
  const r = await fetch(path, options);
  if (r.status === 401) { showLogin(); throw new Error("unauthenticated"); }
  if (!r.ok) {
    const err = new Error((await r.json().catch(() => ({}))).error || r.statusText);
    err.status = r.status;
    throw err;
  }
  return r.json();
}

// Exactly one of the three top-level screens is ever shown.
export function showOnly(id) { for (const s of ["login", "pwchange", "app"]) $(s).hidden = s !== id; }
export function showLogin() {
  session.previewRole = "";
  session.username = "";
  session.isDemo = false;
  if ($("role-preview")) $("role-preview").value = "";
  showOnly("login");
}
export function showApp()   { showOnly("app"); }
export function setBootHandler(fn) { bootHandler = fn; }

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

export function cell(text, cls) {
  const td = document.createElement("td");
  if (cls) td.className = cls;
  td.textContent = text;
  return td;
}

// One error line per view, at the top. A 403 reads as "not permitted", never as an empty table.
export function viewErr(id, e) {
  const p = $(id);
  if (e === null) { p.hidden = true; p.textContent = ""; return; }
  if (e.message === "unauthenticated") return;
  p.hidden = false;
  p.textContent = e.status === 403 ? "Not permitted: " + e.message : e.message;
}

$("go").onclick = async () => {
  $("loginerr").textContent = "";
  $("go").disabled = true;
  try {
    await api(API.login, {
      method: "POST",
      body: JSON.stringify({username: $("u").value, password: $("p").value}),
    });
  } catch (e) {
    $("loginerr").textContent = "Invalid username or password.";
    $("go").disabled = false;
    return;
  }
  // Which screen a valid credential lands on is boot's decision, not the login form's — a failure
  // after this point is not a bad password and must not be reported as one.
  try { await bootHandler(); } finally { $("go").disabled = false; }
};
$("p").onkeydown = e => { if (e.key === "Enter") $("go").click(); };

function applyDemoPresentation(demo) {
  session.demoEnabled = demo?.enabled === true;
  session.demoWindow = session.demoEnabled && demo.dataset_from && demo.dataset_to
    ? {from: demo.dataset_from, to: demo.dataset_to} : null;
  for (const element of document.querySelectorAll("[data-demo-only]"))
    element.hidden = !session.demoEnabled;
  for (const element of document.querySelectorAll("[data-demo-window-only]")) {
    element.hidden = !session.demoWindow;
    element.disabled = !session.demoWindow;
  }
  for (const id of ["trace-range-preset", "flds-range-preset"]) {
    const preset = $(id);
    if (session.demoWindow) preset.value = "demo";
    else if (preset.value === "demo") preset.value = "24";
  }
  if (!session.demoEnabled) return;
  $("demo-username").textContent = demo.username;
  $("demo-password").textContent = demo.password;
  $("u").value = demo.username;
  $("p").value = demo.password;
}

export async function loadDemoEntry() {
  try { applyDemoPresentation(await api(API.demo)); }
  catch (e) { applyDemoPresentation(null); }
}

// Signing out deletes the session server-side. Clearing the view without that would leave a
// credential that still works sitting in the browser.
async function signOut() {
  try { await api(API.logout, {method: "POST"}); } catch (e) { /* already gone is still signed out */ }
  $("u").value = ""; $("p").value = ""; $("who").textContent = "";
  $("tabs").hidden = true;
  // A shown-once token must not outlive the session that minted it on a shared screen.
  $("key-token").hidden = true; $("key-token-val").textContent = "";
  clearPassword();
  showLogin();
  await loadDemoEntry();
}
$("signout").onclick = signOut;
// POST /logout keeps working while a password change is forced, so the way out is still a real
// sign-out rather than an abandoned tab.
$("pw-signout").onclick = signOut;

// ---- password change ---------------------------------------------------------------------------
// One form, #pwkit, moved between the forced screen and Connect (appendChild relocates a node;
// its handlers travel with it). The endpoint is the same either way — being forced changes what is
// around the form, never what it does.

export function clearPassword() {
  for (const id of ["pw-cur", "pw-new", "pw-new2"]) $(id).value = "";
  $("pw-msg").className = "err";
  $("pw-msg").textContent = "";
}

$("pw-go").onclick = async () => {
  const msg = $("pw-msg");
  msg.className = "err";
  msg.textContent = "";
  const cur = $("pw-cur").value, next = $("pw-new").value;
  // The two client-side checks are the two the server cannot make for us: it never sees the
  // confirmation field, and an empty new password is a mistake worth catching before a round trip.
  if (!next) { msg.textContent = "Choose a new password."; return; }
  if (next !== $("pw-new2").value) { msg.textContent = "The two new passwords do not match."; return; }
  $("pw-go").disabled = true;
  try {
    await api(API.password, { method: "POST",
      body: JSON.stringify({ current_password: cur, new_password: next }) });
  } catch (e) {
    // Verbatim: a wrong current password (403, not 401 — the session is fine, the credential in
    // the form is not), or a new one the server refuses, is the server's to say.
    msg.textContent = e.message;
    $("pw-go").disabled = false;
    return;
  }
  clearPassword();
  if (session.passwordForced) {
    session.passwordForced = false;
    $("pw-go").disabled = false;
    await bootHandler();
    return;
  }
  msg.className = "muted";
  msg.textContent = "Password changed. Your other sessions have been signed out.";
  $("pw-go").disabled = false;
};
$("pw-new2").onkeydown = e => { if (e.key === "Enter") $("pw-go").click(); };
