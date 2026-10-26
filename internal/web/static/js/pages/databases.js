import { api } from "../api.js";
import { h, pageHead, btn, table, empty, status, ago, modal, field, errorBox, withBusy, errToast } from "../ui.js";
import { credentialFields } from "./creds.js";

const backupCell = (d) => (d.last_backup ? h("span", {}, status(d.last_backup.status), d.last_backup.finished_at ? h("span", { class: "hint ms-2" }, ago(d.last_backup.finished_at)) : null) : h("span", { class: "hint" }, "chưa có"));

export default async function databases(ctx) {
  const me = ctx.me;
  const body = h("div", {});
  ctx.root.append(pageHead("Databases", "Các node database của bạn", btn("Tạo database", "btn-primary", () => createDialog(ctx), "plus-lg")), body);

  async function load() {
    const rows = await api("GET", "/api/databases");
    const cols = [
      { title: "Tên", cell: (d) => h("a", { href: "#/databases/" + encodeURIComponent(d.name), class: "fw-semibold" }, d.name) },
      { title: "Engine", cell: (d) => `${d.engine} ${d.version}` },
      { title: "Domain", cell: (d) => h("span", { class: "mono" }, d.domain) },
      { title: "Trạng thái", cell: (d) => status(d.status, { pulse: true }) },
      { title: "Backup gần nhất", cell: backupCell },
    ];
    if (me.role === "admin") cols.push({ title: "Owner", cell: (d) => d.owner });
    body.replaceChildren(table(cols, rows, empty("database", "Chưa có database nào", btn("Tạo database đầu tiên", "btn-primary btn-sm", () => createDialog(ctx)))));
    return rows.some((d) => d.status === "creating");
  }

  let timer;
  const tick = async () => {
    try {
      if (await load()) timer = setTimeout(tick, 3000);
    } catch (e) {
      if (ctx.alive()) errToast(e);
    }
  };
  ctx.onCleanup(() => clearTimeout(timer));
  await tick();
}

async function createDialog(ctx) {
  const [engines, domains] = await Promise.all([api("GET", "/api/engines"), api("GET", "/api/domains")]);
  const mine = domains.filter((d) => d.owner === ctx.me.username);
  const eng = h("select", { class: "form-select", name: "engine" }, engines.map((e) => h("option", { value: e.name }, e.name)));
  const ver = h("select", { class: "form-select", name: "version" });
  const fill = () => {
    const e = engines.find((x) => x.name === eng.value);
    ver.replaceChildren(...e.versions.map((v) => h("option", { value: v, selected: v === e.default }, v)));
  };
  eng.addEventListener("change", fill);
  fill();
  const dom = h("select", { class: "form-select", name: "domain" }, mine.map((d) => h("option", { value: d.name }, d.name)));
  const creds = credentialFields(ctx.me.username);
  const err = errorBox();
  const go = h("button", { type: "submit", class: "btn btn-primary" }, "Tạo database");
  const form = h(
    "form",
    { novalidate: true },
    h("div", { class: "row" }, h("div", { class: "col-6" }, field("Engine", eng)), h("div", { class: "col-6" }, field("Version", ver))),
    creds.nodes[0],
    mine.length
      ? field("Domain", dom, "Client kết nối bằng tên này (PostgreSQL, Redis, MongoDB chọn database theo tên này; MySQL/MariaDB chọn theo user và dùng tên này trong chứng chỉ). Chỉ chọn được domain bạn đã đăng ký.")
      : h("div", { class: "alert alert-warning" }, "Bạn chưa đăng ký domain nào. ", h("a", { href: "#/domains", "data-bs-dismiss": "modal" }, "Đăng ký domain"), " trước khi tạo database."),
    creds.nodes.slice(1),
    err.el,
  );
  const m = modal({ title: "Tạo database", body: form, footer: [h("button", { type: "button", class: "btn btn-outline-secondary", "data-bs-dismiss": "modal" }, "Hủy"), go] });
  go.addEventListener("click", () => form.requestSubmit());
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.set("");
    const bad = creds.check();
    if (bad) return err.set(bad);
    if (!dom.value) return err.set("Chọn một domain");
    await withBusy(go, async () => {
      try {
        const v = creds.values();
        const body = { engine: eng.value, version: ver.value, domain: dom.value, name: v.name };
        if (v.db_user) body.db_user = v.db_user;
        if (v.db_password) body.db_password = v.db_password;
        const d = await api("POST", "/api/databases", body);
        m.hide();
        ctx.navigate("#/databases/" + encodeURIComponent(d.name));
      } catch (e2) {
        err.set(e2.message);
      }
    });
  });
  m.el.addEventListener("shown.bs.modal", () => creds.name.focus());
}

