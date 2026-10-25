import { api, download } from "../api.js";
import { h, pageHead, btn, table, empty, fmtTime, field, input, errToast } from "../ui.js";

const detailText = (d) => {
  const s = Object.entries(d || {})
    .map(([k, v]) => `${k}=${typeof v === "object" ? JSON.stringify(v) : v}`)
    .join(" ");
  return s.length > 90 ? s.slice(0, 89) + "…" : s;
};

export default async function audit(ctx) {
  const f = {
    actor: input({ name: "actor", placeholder: "tên đăng nhập" }),
    action: input({ name: "action", placeholder: "vd: db. hoặc backup.start" }),
    db: input({ name: "db", placeholder: "tên database" }),
    from: input({ name: "from", type: "datetime-local" }),
    to: input({ name: "to", type: "datetime-local" }),
  };
  const body = h("div", {});
  const more = h("div", { class: "mt-3 text-center" });
  const filters = h(
    "form",
    { class: "panel mb-3", novalidate: true },
    h(
      "div",
      { class: "panel-body" },
      h("div", { class: "row g-2" }, h("div", { class: "col-md-3" }, field("Người", f.actor)), h("div", { class: "col-md-3" }, field("Hành động", f.action)), h("div", { class: "col-md-3" }, field("Database", f.db)), h("div", { class: "col-md-3 d-flex align-items-end pb-3" }, h("button", { type: "submit", class: "btn btn-primary w-100" }, "Lọc"))),
      h("div", { class: "row g-2" }, h("div", { class: "col-md-3" }, field("Từ", f.from)), h("div", { class: "col-md-3" }, field("Đến", f.to))),
    ),
  );
  ctx.root.append(
    pageHead("Audit log", ctx.me.role === "admin" ? "Mọi hành động trong hệ thống" : "Hành động của bạn và mọi việc xảy ra trên database bạn sở hữu", btn("Xuất CSV", "btn-outline-secondary", exportCSV, "filetype-csv")),
    filters,
    body,
    more,
  );

  const params = () => {
    const p = new URLSearchParams();
    for (const [k, el] of Object.entries(f)) {
      if (!el.value.trim()) continue;
      p.set(k, k === "from" || k === "to" ? new Date(el.value).toISOString() : el.value.trim());
    }
    return p;
  };

  let rows = [];
  async function fetchPage(before) {
    const p = params();
    p.set("limit", "100");
    if (before) p.set("before", String(before));
    return api("GET", "/api/audit?" + p);
  }
  function draw(next) {
    body.replaceChildren(
      table(
        [
          { title: "Thời gian", cell: (e) => fmtTime(e.at), cls: "nowrap" },
          { title: "Người", cell: (e) => e.actor },
          { title: "IP", cell: (e) => h("span", { class: "mono" }, e.ip || "—") },
          { title: "Hành động", cell: (e) => h("span", { class: "mono" }, e.action) },
          { title: "Đối tượng", cell: (e) => (e.target_type ? `${e.target_type}${e.target_id ? ":" + e.target_id : ""}` : "—") },
          { title: "Database", cell: (e) => e.db || "—" },
          { title: "Kết quả", cell: (e) => (e.success ? h("span", { class: "status ok" }, "ok") : h("span", { class: "status failed" }, "bị từ chối/lỗi")) },
          { title: "Chi tiết", cell: (e) => h("span", { class: "hint mono" }, detailText(e.detail)) },
        ],
        rows,
        empty("journal-text", "Không có bản ghi nào khớp bộ lọc"),
      ),
    );
    more.replaceChildren(next ? btn("Tải thêm", "btn-outline-secondary", () => loadMore(next), "chevron-down") : "");
  }
  async function reload() {
    const r = await fetchPage(0);
    rows = r.entries;
    draw(r.next_before);
  }
  async function loadMore(before) {
    const r = await fetchPage(before);
    rows = rows.concat(r.entries);
    draw(r.next_before);
  }
  async function exportCSV() {
    try {
      await download("/api/audit.csv?" + params(), "audit.csv");
    } catch (e) {
      errToast(e);
    }
  }
  filters.addEventListener("submit", (e) => {
    e.preventDefault();
    reload().catch(errToast);
  });
  await reload();
}
