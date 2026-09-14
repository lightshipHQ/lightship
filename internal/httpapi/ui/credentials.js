import { $, API, api, cell, el, session, viewErr } from "./core.js";

// ---- API keys ----------------------------------------------------------------------------------
// Connect owns key creation, the shown-once token box, and password changes.

$("key-mint").onclick = async () => {
  $("key-err").hidden = true;
  $("key-mint").disabled = true;
  const body = { name: $("key-name").value.trim() };
  if ($("key-expiry").value) body.expires_in = $("key-expiry").value;
  let d;
  try {
    d = await api(API.keys, { method: "POST", body: JSON.stringify(body) });
  } catch (e) {
    $("key-err").hidden = false;
    $("key-err").textContent = e.message;
    $("key-mint").disabled = false;
    return;
  }
  $("key-token").hidden = false;
  $("key-token-val").textContent = d.token;
  $("key-name").value = "";
  $("key-copy").onclick = () => copyText(d.token, $("key-copy"));
  if (!$("view-keys").hidden) refreshKeyRows().catch(e => viewErr("keys-err", e));
  $("key-mint").disabled = false;
};

export async function loadKeys() {
  viewErr("keys-err", null);
  $("keys-note").textContent = "Loading API keys…";
  $("keys-rows").replaceChildren();
  $("keys-pwslot").appendChild($("pwkit")); // reclaim the shared password form (see above)
  $("password-section").hidden = session.isDemo;
  $("key-expiry").value = session.demoEnabled ? "24h" : $("key-expiry").value;
  $("key-expiry").disabled = session.demoEnabled;
  $("key-demo-note").hidden = !session.demoEnabled;
  $("keys-listtitle").textContent = session.isAdmin ? "All API keys" : "Your API keys";
  const endpoint = location.origin + "/mcp";
  $("mcp-url").textContent = endpoint;
  $("mcp-copy").onclick = () => copyText(endpoint, $("mcp-copy"));
  try {
    const count = await refreshKeyRows();
    if (!$("keys-section").dataset.initialized) {
      $("keys-section").open = count === 0;
      $("keys-section").dataset.initialized = "true";
    }
  } catch (e) { viewErr("keys-err", e); }
}

async function refreshKeyRows() {
  const d = await api(API.keys);
  renderKeyRows(d.keys || []);
  return (d.keys || []).length;
}

const fmtWhen = t => new Date(t).toLocaleString();

function renderKeyRows(keys) {
  const head = $("keys-head");
  head.replaceChildren();
  // For a non-admin every key acts as themselves, so the column says nothing and is dropped.
  for (const c of ["name", ...(session.isAdmin ? ["acts as"] : []), "created", "expires", "last used", ""])
    head.appendChild(el("th", "", c));

  const host = $("keys-rows");
  host.replaceChildren();
  for (const k of keys) {
    const dead = !!k.revoked_at;
    const tr = document.createElement("tr");

    const name = document.createElement("td");
    name.appendChild(el("span", dead ? "faint" : "", k.name));
    if (dead) name.appendChild(el("span", "badge", "revoked " + fmtWhen(k.revoked_at)));
    else if (k.expires_at && new Date(k.expires_at) < new Date())
      name.appendChild(el("span", "badge", "expired"));
    tr.appendChild(name);

    if (session.isAdmin) tr.appendChild(cell(k.username, dead ? "mono faint" : "mono"));
    tr.appendChild(cell(fmtWhen(k.created_at), dead ? "faint" : "muted"));
    tr.appendChild(cell(k.expires_at ? fmtWhen(k.expires_at) : "never", dead ? "faint" : "muted"));
    tr.appendChild(cell(k.last_used_at ? fmtWhen(k.last_used_at) : "never used",
      dead || !k.last_used_at ? "faint" : "muted"));

    const act = document.createElement("td");
    if (!dead) {
      const b = el("button", "", "Revoke");
      b.onclick = async () => {
        if (!confirm(`Revoke "${k.name}"? Anything using this key stops working immediately, ` +
          "and there is no way to undo it.")) return;
        try {
          await api(API.key(k.id), { method: "DELETE" });
          await refreshKeyRows();
        } catch (e) {
          viewErr("keys-err", new Error("Could not revoke the API key: " + e.message));
        }
      };
      act.appendChild(b);
    }
    tr.appendChild(act);
    host.appendChild(tr);
  }
  $("keys-note").textContent = keys.length
    ? "" : "No API keys yet. Create one above to connect an AI tool.";
}

function copyText(text, btn) {
  const done = () => { const t = btn.textContent; btn.textContent = "Copied"; setTimeout(() => btn.textContent = t, 1500); };
  navigator.clipboard?.writeText(text).then(done).catch(() => {
    const ta = document.createElement("textarea");
    ta.value = text; document.body.appendChild(ta); ta.select();
    document.execCommand("copy"); ta.remove(); done();
  });
}
