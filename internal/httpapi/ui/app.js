// ---- boot --------------------------------------------------------------------------------------
import { $, API, api, clearPassword, loadDemoEntry, session, setBootHandler, setSetupProgress, showApp, showLogin, showOnly } from "./core.js";
import { initWindow, load, reset } from "./traces.js";
import { hasUnsavedFieldChanges, loadSchemaView } from "./setup.js";
import { loadKeys } from "./credentials.js";
import { loadAudit, loadRoles, loadUsers } from "./admin.js";

const loaders = { keys: loadKeys, schema: loadSchemaView, roles: loadRoles, users: loadUsers,
  audit: loadAudit };
const setupViews = new Set(["schema", "roles", "users"]);
let identity = null;
let currentView = "";

function applyIdentity(me) {
  if (me) identity = me;
  session.username = identity?.username || session.username;
  session.actorIsAdmin = !!(identity?.actor_is_admin ?? identity?.is_admin);
  session.isAdmin = !!identity?.is_admin && !session.previewRole;
  session.isDemo = !!identity?.is_demo;
  $("who").textContent = session.username;
  $("role-preview-control").hidden = !session.actorIsAdmin;
  $("preview-banner").hidden = !session.previewRole;
  $("preview-exit").hidden = !session.previewRole;
  $("preview-banner-title").textContent = session.previewRole
    ? `Previewing ${session.previewRole}.` : "";
  $("preview-banner-detail").textContent = session.previewRole
    ? "You are seeing trace access as this role; administrative changes are unavailable." : "";
  for (const tab of document.querySelectorAll(".tab[data-admin]")) tab.hidden = !session.isAdmin;
  for (const tab of document.querySelectorAll(".tab[data-preview-hidden]"))
    tab.hidden = !!session.previewRole;
}

async function loadPreviewRoles(force = false) {
  if (!session.actorIsAdmin) return null;
  const select = $("role-preview");
  if (!force && select.children.length > 1) return;
  try {
    const data = await api(API.schema);
    setSetupProgress(data.model);
    select.replaceChildren(new Option("Admin interface", ""));
    for (const role of data.model?.roles || []) {
      if (role.name !== "admin") select.appendChild(new Option(role.name, role.name));
    }
    const selected = session.previewRole;
    select.value = [...select.options].some(option => option.value === selected) ? selected : "";
    if (selected && !select.value) await setPreviewRole("");
    return data.model;
  } catch (error) {
    $("preview-banner").hidden = false;
    $("preview-exit").hidden = true;
    $("preview-banner-title").textContent = "Role preview is unavailable.";
    $("preview-banner-detail").textContent = "Could not refresh the available roles.";
    return null;
  }
}

function showView(name) {
  if (currentView === "schema" && name !== "schema" && hasUnsavedFieldChanges() &&
      !confirm("Discard unsaved field changes?")) return false;
  const inSetup = setupViews.has(name);
  for (const t of document.querySelectorAll(".tab"))
    t.classList.toggle("active", t.dataset.view === name ||
      (t.dataset.setupRoot !== undefined && inSetup));
  for (const t of document.querySelectorAll(".tab")) {
    const active = t.classList.contains("active");
    if (active) t.setAttribute("aria-current", "page");
    else t.removeAttribute("aria-current");
  }
  $("view-list").hidden = name !== "traces";
  $("view-detail").hidden = true;
  $("winctl").hidden = name !== "traces";
  for (const v of Object.keys(loaders)) $("view-" + v).hidden = v !== name;
  currentView = name;
  if (loaders[name]) loaders[name]();
  return true;
}
for (const t of document.querySelectorAll(".tab"))
  t.onclick = () => showView(t.dataset.view);

// One call answers all three questions the page opens with: is anyone signed in (a 401 says no),
// who are they, and may they see the admin tabs. It used to provoke a 403 on /schema to learn the
// last of those, which cost every non-admin an extra request and an error in their console.
// The server gates every endpoint regardless — the nav is convenience, not enforcement.
async function boot() {
  const me = await api(API.me);
  applyIdentity(me);
  // While a change is required the server answers 403 to everything else, so the page shows that
  // screen alone rather than an application whose every tab would fail.
  if (me.must_change_password) {
    session.passwordForced = true;
    $("tabs").hidden = true;
    $("pwchange-slot").appendChild($("pwkit"));
    clearPassword();
    showOnly("pwchange");
    return;
  }
  session.passwordForced = false;
  // Everyone gets Traces and Connect — minting your own key is how any user connects an agent.
  $("tabs").hidden = false;
  await loadPreviewRoles();
  initWindow(); showApp(); showView("traces"); reset(); load();
}

async function setPreviewRole(role) {
  const previous = session.previewRole;
  session.previewRole = role;
  $("role-preview").value = role;
  applyIdentity();
  if (!showView("traces")) {
    session.previewRole = previous;
    $("role-preview").value = previous;
    applyIdentity();
    return;
  }
  reset();
  load();
}

$("role-preview").onfocus = () => loadPreviewRoles(true);
$("role-preview").onchange = () => setPreviewRole($("role-preview").value);
$("preview-exit").onclick = () => setPreviewRole("");

// A 401 on that first call is how the page learns nobody is signed in.
setBootHandler(boot);
(async () => {
  await loadDemoEntry();
  try { await boot(); } catch (e) { showLogin(); }
})();
