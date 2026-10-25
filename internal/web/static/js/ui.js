// Small DOM helpers. Text always goes in as text nodes, never as HTML.

export function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
    else if (k === "value") el.value = v;
    else if (v === true) el.setAttribute(k, "");
    else el.setAttribute(k, v);
  }
  add(el, kids);
  return el;
}

export function add(el, kids) {
  for (const k of kids.flat(Infinity)) {
    if (k == null || k === false) continue;
    el.append(k instanceof Node ? k : document.createTextNode(String(k)));
  }
  return el;
}

export const icon = (name, cls = "") => h("i", { class: `bi bi-${name} ${cls}`.trim(), "aria-hidden": "true" });

export function clear(el) {
  el.replaceChildren();
  return el;
}

// ---- toast ----

let toastWrap;
export function toast(msg, isErr = false) {
  if (!toastWrap) {
    toastWrap = h("div", { class: "toast-wrap", role: "status", "aria-live": "polite" });
    document.body.append(toastWrap);
  }
  const t = h("div", { class: "toast-msg" + (isErr ? " err" : "") }, msg);
  toastWrap.append(t);
  setTimeout(() => t.remove(), isErr ? 8000 : 3500);
}

export function errToast(e) {
  toast(e && e.message ? e.message : String(e), true);
}

// ---- formatting ----

const pad = (n) => String(n).padStart(2, "0");

export function fmtTime(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export function ago(iso) {
  if (!iso) return "—";
  const s = Math.max(0, Math.round((Date.now() - new Date(iso)) / 1000));
  if (s < 60) return `${s} giây trước`;
  if (s < 3600) return `${Math.floor(s / 60)} phút trước`;
  if (s < 86400) return `${Math.floor(s / 3600)} giờ trước`;
  return `${Math.floor(s / 86400)} ngày trước`;
}

export function elapsed(iso) {
  const s = Math.max(0, Math.round((Date.now() - new Date(iso)) / 1000));
  return s < 60 ? `${s}s` : s < 3600 ? `${Math.floor(s / 60)}m ${s % 60}s` : `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}

export function fmtBytes(n) {
  if (!n) return "—";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)} ${u[i]}`;
}

const STATUS_VI = { running: "đang chạy", ok: "xong", failed: "lỗi", error: "lỗi", creating: "đang tạo", pending: "chờ", deleted: "đã xóa" };

export function status(s, { pulse = false } = {}) {
  return h("span", { class: `status ${s}${pulse && (s === "running" || s === "creating") ? " pulse" : ""}` }, STATUS_VI[s] || s);
}

// ---- clipboard, random ----

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("Đã sao chép");
  } catch {
    // Not a secure context or clipboard blocked: fall back to selecting text.
    const ta = h("textarea", { class: "visually-hidden" });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    toast(ok ? "Đã sao chép" : "Không sao chép được — hãy chọn và copy tay", !ok);
  }
}

export function randomString(alphabet, n) {
  const out = [];
  const limit = 256 - (256 % alphabet.length);
  while (out.length < n) {
    for (const b of crypto.getRandomValues(new Uint8Array(n * 2))) {
      if (b < limit && out.length < n) out.push(alphabet[b % alphabet.length]);
    }
  }
  return out.join("");
}

export const randomSuffix = () => randomString("abcdefghijklmnopqrstuvwxyz0123456789", 8);
export const randomPassword = () => randomString("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~", 24);

// ---- modal ----

// modal shows a Bootstrap modal. body is a Node; footer a list of Nodes. Returns {el, hide}.
export function modal({ title, body, footer = [], size = "" }) {
  const el = h(
    "div",
    { class: "modal fade", tabindex: "-1", "aria-hidden": "true" },
    h(
      "div",
      { class: `modal-dialog modal-dialog-scrollable ${size}`.trim() },
      h(
        "div",
        { class: "modal-content" },
        h("div", { class: "modal-header" }, h("h2", { class: "modal-title fs-5" }, title), h("button", { type: "button", class: "btn-close", "data-bs-dismiss": "modal", "aria-label": "Đóng" })),
        h("div", { class: "modal-body" }, body),
        footer.length ? h("div", { class: "modal-footer" }, footer) : null,
      ),
    ),
  );
  document.body.append(el);
  const m = new window.bootstrap.Modal(el);
  el.addEventListener("hidden.bs.modal", () => {
    m.dispose();
    el.remove();
  });
  m.show();
  return { el, hide: () => m.hide() };
}

export function confirmDialog({ title, body, okLabel = "Đồng ý", danger = false }) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (v) => {
      if (done) return;
      done = true;
      resolve(v);
    };
    const ok = h("button", { type: "button", class: `btn ${danger ? "btn-danger" : "btn-primary"}`, onclick: () => (finish(true), m.hide()) }, okLabel);
    const cancel = h("button", { type: "button", class: "btn btn-outline-secondary", "data-bs-dismiss": "modal" }, "Hủy");
    const m = modal({ title, body: typeof body === "string" ? h("p", { class: "mb-0" }, body) : body, footer: [cancel, ok] });
    m.el.addEventListener("hidden.bs.modal", () => finish(false));
  });
}

// ---- forms ----

let uid = 0;
export function field(label, input, hint) {
  const id = input.id || `f${++uid}`;
  input.id = id;
  return h("div", { class: "mb-3" }, h("label", { class: "form-label", for: id }, label), input, hint ? h("div", { class: "hint mt-1" }, hint) : null);
}

export function input(attrs = {}) {
  return h("input", { class: "form-control", autocomplete: "off", spellcheck: "false", ...attrs });
}

// withBusy disables button while fn runs.
export async function withBusy(button, fn) {
  button.disabled = true;
  try {
    return await fn();
  } finally {
    button.disabled = false;
  }
}

// ---- tables and empty states ----

export function empty(iconName, text, action) {
  return h("div", { class: "empty" }, icon(iconName), h("div", {}, text), action ? h("div", { class: "mt-2" }, action) : null);
}

// table builds a panel with a table. cols: [{title, cell(row) -> Node|string, cls}]
export function table(cols, rows, emptyNode) {
  if (!rows.length) return h("div", { class: "panel" }, emptyNode || empty("inbox", "Chưa có gì ở đây"));
  return h(
    "div",
    { class: "panel table-wrap" },
    h(
      "table",
      { class: "table table-hover" },
      h("thead", {}, h("tr", {}, cols.map((c) => h("th", { scope: "col" }, c.title)))),
      h("tbody", {}, rows.map((r) => h("tr", {}, cols.map((c) => h("td", { class: c.cls || "" }, c.cell(r)))))),
    ),
  );
}

export function pageHead(title, sub, ...actions) {
  return h(
    "div",
    { class: "page-head" },
    h("div", {}, h("h1", {}, title), sub ? h("div", { class: "sub" }, sub) : null),
    actions.length ? h("div", { class: "actions" }, actions) : null,
  );
}

export function btn(label, cls, onclick, iconName) {
  return h("button", { type: "button", class: `btn ${cls}`, onclick }, iconName ? [icon(iconName), " "] : null, label);
}

// strip is the dark copyable line used for connection strings and secrets.
export function strip(value, { masked = false } = {}) {
  const v = h("span", { class: "value" }, value);
  return h("div", { class: "strip" }, v, h("button", { type: "button", class: "btn btn-sm btn-outline-secondary", title: "Sao chép", "aria-label": "Sao chép", onclick: () => copyText(v.textContent) }, icon("clipboard")));
}

// gate renders a full-screen card (setup, login, invite) and returns the card element.
export function gate(root, lead, ...kids) {
  const card = h("div", { class: "card-gate" }, h("div", { class: "brand" }, icon("snow"), "Fridge"), h("p", { class: "lead" }, lead), kids);
  root.replaceChildren(h("main", { class: "gate" }, card));
  return card;
}

export const USERNAME_RE = /^[a-z][a-z0-9]{2,15}$/;
export const USERNAME_HINT = "chữ thường và số, 3–16 ký tự, bắt đầu bằng chữ";

// passwordPair builds two password inputs (new + repeat) and a check() returning an error text or "".
export function passwordPair(label = "Mật khẩu") {
  const a = input({ type: "password", autocomplete: "new-password", name: "new-password" });
  const b = input({ type: "password", autocomplete: "new-password", name: "repeat-password" });
  return {
    nodes: [field(label, a, "Ít nhất 12 ký tự. Mật khẩu được xử lý trong trình duyệt và không bao giờ gửi đi."), field("Nhập lại mật khẩu", b)],
    value: () => a.value,
    check() {
      if (a.value.length < 12) return "Mật khẩu cần ít nhất 12 ký tự";
      if (a.value !== b.value) return "Hai mật khẩu không giống nhau";
      return "";
    },
  };
}

export function errorBox() {
  const el = h("div", { class: "err-text", role: "alert" });
  return { el, set: (m) => (el.textContent = m || "") };
}
