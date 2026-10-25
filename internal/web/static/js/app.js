import { api, setUnauthorizedHandler } from "./api.js";
import { route, match, query, navigate } from "./router.js";
import { h, icon, empty } from "./ui.js";
import { mountShell } from "./layout.js";

const root = document.getElementById("app");

route("#/setup", () => import("./pages/setup.js"), { public: true });
route("#/login", () => import("./pages/login.js"), { public: true });
route("#/invite/:token", () => import("./pages/invite.js"), { public: true });
route("#/password", () => import("./pages/password.js"), { nav: "" });
route("#/databases", () => import("./pages/databases.js"), { nav: "databases" });
route("#/databases/:name", () => import("./pages/database.js"), { nav: "databases" });
route("#/restore", () => import("./pages/restore.js"), { nav: "restore" });
route("#/destinations", () => import("./pages/destinations.js"), { nav: "destinations" });
route("#/domains", () => import("./pages/domains.js"), { nav: "domains" });
route("#/audit", () => import("./pages/audit.js"), { nav: "audit" });
route("#/settings", () => import("./pages/settings.js"), { nav: "settings" });
route("#/users", () => import("./pages/users.js"), { nav: "users", admin: true });
route("#/sessions", () => import("./pages/sessions.js"), { nav: "sessions", admin: true });
route("#/images", () => import("./pages/images.js"), { nav: "images", admin: true });
route("#/activity", () => import("./pages/activity.js"), { nav: "activity", admin: true });

const state = { me: null, loaded: false, setupRequired: false, shell: null };
let cleanups = [];
let seq = 0;

function stopPage() {
  for (const f of cleanups.splice(0)) {
    try {
      f();
    } catch {
      /* a failing cleanup must not block navigation */
    }
  }
}

async function loadMe() {
  try {
    state.me = await api("GET", "/api/me", undefined, { quiet: true });
    state.setupRequired = false;
  } catch (e) {
    if (e.status !== 401) throw e;
    state.me = null;
    state.setupRequired = e.message === "setup required";
  }
  state.loaded = true;
}

function signedOut(msg) {
  state.me = null;
  state.loaded = true;
  state.setupRequired = msg === "setup required";
  state.shell = null;
  if (!/^#\/(login|setup|invite\/)/.test(location.hash)) navigate(state.setupRequired ? "#/setup" : "#/login");
}
setUnauthorizedHandler(signedOut);

async function logout() {
  try {
    await api("POST", "/api/auth/logout", undefined, { quiet: true });
  } catch {
    /* already gone */
  }
  state.loaded = false;
  state.shell = null;
  navigate("#/login");
}

function placeholder(el, text) {
  el.replaceChildren(h("div", { class: "panel" }, empty("cone-striped", text)));
}

async function render() {
  const mine = ++seq;
  stopPage();
  const hash = location.hash || "#/";
  try {
    if (!state.loaded) await loadMe();
  } catch (e) {
    root.replaceChildren(h("div", { class: "gate" }, h("div", { class: "card-gate" }, h("div", { class: "brand" }, icon("snow"), "Fridge"), h("p", { class: "err-text" }, "Không kết nối được máy chủ: " + e.message))));
    return;
  }
  if (mine !== seq) return;
  const m = match(hash);

  if (m && m.route.isPublic) {
    if (state.me && !hash.startsWith("#/invite/")) return navigate("#/databases");
    state.shell = null;
    return runPage(m, root, mine, null);
  }
  if (!state.me) return navigate(state.setupRequired ? "#/setup" : "#/login");
  if (!m || hash === "#/" || (m.route.admin && state.me.role !== "admin")) return navigate("#/databases");

  if (!state.shell) state.shell = mountShell(root, state.me, logout);
  state.shell.setActive(m.route.nav);
  state.shell.imageDot();
  return runPage(m, state.shell.content, mine, state.me);
}

async function runPage(m, el, mine, me) {
  el.replaceChildren();
  window.scrollTo(0, 0);
  let mod;
  try {
    mod = await m.route.load();
  } catch {
    return placeholder(el, "Đang xây dựng");
  }
  if (mine !== seq) return;
  const ctx = {
    root: el,
    me,
    params: m.params,
    query: query(location.hash),
    navigate,
    onCleanup: (f) => cleanups.push(f),
    alive: () => mine === seq,
    reloadMe: async () => {
      await loadMe();
    },
    signedIn: async () => {
      state.loaded = false;
      state.shell = null;
      navigate("#/databases");
    },
  };
  try {
    await mod.default(ctx);
  } catch (e) {
    if (mine === seq && e.status !== 401) el.replaceChildren(h("div", { class: "panel" }, h("div", { class: "panel-body" }, h("p", { class: "err-text mb-0" }, "Lỗi: " + e.message))));
  }
}

window.addEventListener("hashchange", render);
render();
