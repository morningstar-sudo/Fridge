// The signed-in shell: collapsible sidebar and a content area.
import { h, icon } from "./ui.js";
import { api } from "./api.js";

const NAV = [
  { id: "databases", href: "#/databases", icon: "database", label: "Databases" },
  { id: "restore", href: "#/restore", icon: "arrow-counterclockwise", label: "Restore" },
  { id: "destinations", href: "#/destinations", icon: "cloud-arrow-up", label: "Destinations" },
  { id: "domains", href: "#/domains", icon: "globe2", label: "Domains" },
  { id: "audit", href: "#/audit", icon: "journal-text", label: "Audit log" },
  { id: "settings", href: "#/settings", icon: "gear", label: "Settings" },
];
const ADMIN_NAV = [
  { id: "users", href: "#/users", icon: "people", label: "Users" },
  { id: "sessions", href: "#/sessions", icon: "shield-lock", label: "Sessions" },
  { id: "images", href: "#/images", icon: "boxes", label: "Images" },
  { id: "activity", href: "#/activity", icon: "activity", label: "Hoạt động" },
];

const KEY = "fridge.sidebar";
const read = () => {
  try {
    return localStorage.getItem(KEY);
  } catch {
    return null;
  }
};
const write = (v) => {
  try {
    localStorage.setItem(KEY, v);
  } catch {
    /* storage blocked: the sidebar just does not remember */
  }
};

// mountShell renders the shell into root and returns handles for the app.
export function mountShell(root, me, onLogout) {
  const shell = h("div", { class: "shell" + (read() === "slim" ? " slim" : "") });
  const links = new Map();
  const link = (n) => {
    const a = h("a", { class: "nav-link", href: n.href, title: n.label }, icon(n.icon), h("span", { class: "label" }, n.label));
    links.set(n.id, a);
    return a;
  };
  const toggle = h("button", { type: "button", class: "btn btn-sm", "aria-label": "Đóng/mở thanh bên", "aria-expanded": String(!shell.classList.contains("slim")) }, icon("list", "fs-5"));
  toggle.addEventListener("click", () => {
    if (window.matchMedia("(max-width: 767px)").matches) {
      shell.classList.toggle("open");
      return;
    }
    const slim = shell.classList.toggle("slim");
    write(slim ? "slim" : "full");
    toggle.setAttribute("aria-expanded", String(!slim));
  });
  const content = h("div", { class: "page", id: "content" });
  const logout = h("button", { type: "button", class: "nav-link btn btn-link text-start w-100", title: "Đăng xuất" }, icon("box-arrow-right"), h("span", { class: "label" }, "Đăng xuất"));
  logout.addEventListener("click", onLogout);
  const sidebar = h(
    "aside",
    { class: "sidebar", "aria-label": "Điều hướng chính" },
    h("a", { class: "brand", href: "#/databases" }, icon("snow"), h("span", {}, "Fridge")),
    h("nav", {}, NAV.map(link), me.role === "admin" ? [h("div", { class: "nav-sep" }), ADMIN_NAV.map(link)] : null),
    h(
      "div",
      { class: "foot" },
      h("div", { class: "who" }, icon("person-circle", "fs-5"), h("div", { class: "who-text" }, h("div", {}, me.username), h("small", {}, me.role === "admin" ? "quản trị viên" : "người dùng"))),
      h("a", { class: "nav-link", href: "#/password", title: "Đổi mật khẩu" }, icon("key"), h("span", { class: "label" }, "Đổi mật khẩu")),
      logout,
    ),
  );
  shell.addEventListener("click", (e) => {
    if (e.target.closest(".nav-link") && window.matchMedia("(max-width: 767px)").matches) shell.classList.remove("open");
  });
  shell.append(sidebar, h("div", { class: "main" }, h("div", { class: "topbar" }, toggle), content));
  root.replaceChildren(shell);

  let dotChecked = 0;
  return {
    content,
    setActive(id) {
      for (const [k, a] of links) {
        if (k === id) a.setAttribute("aria-current", "page");
        else a.removeAttribute("aria-current");
        a.classList.toggle("active", k === id);
      }
    },
    // A dot on Images when some image has a newer version (checked at most once a minute).
    async imageDot() {
      if (me.role !== "admin" || Date.now() - dotChecked < 60000) return;
      dotChecked = Date.now();
      try {
        const rows = await api("GET", "/api/images", undefined, { quiet: true });
        const a = links.get("images");
        a.querySelector(".dot")?.remove();
        if (rows.some((r) => r.update && r.update.available)) a.append(h("span", { class: "dot", title: "Có phiên bản mới" }));
      } catch {
        /* the dot is a courtesy */
      }
    },
  };
}
