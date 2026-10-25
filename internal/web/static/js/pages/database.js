import { api, download } from "../api.js";
import { h, icon, pageHead, btn, table, empty, status, ago, fmtTime, fmtBytes, modal, field, input, errorBox, withBusy, errToast, toast, confirmDialog, strip, copyText } from "../ui.js";

const TABS = [
  ["conn", "Kết nối"],
  ["backups", "Backups"],
  ["schedules", "Lịch backup"],
  ["members", "Người quản lý"],
];

export default async function database(ctx) {
  const name = ctx.params.name;
  let d = await api("GET", "/api/databases/" + encodeURIComponent(name));
  const isOwner = ctx.me.role === "admin" || d.owner === ctx.me.username;
  const [dests, engines] = await Promise.all([api("GET", "/api/destinations"), api("GET", "/api/engines")]);
  const destName = (id) => dests.find((x) => x.id === id)?.name || "#" + id;
  const port = engines.find((e) => e.name === d.engine)?.public_port || 5432;

  const statusEl = h("span", {});
  const renderStatus = () => statusEl.replaceChildren(status(d.status, { pulse: true }));
  renderStatus();
  const tabsEl = h("ul", { class: "nav nav-tabs", role: "tablist" });
  const pane = h("div", { role: "tabpanel" });
  let active = ctx.query.get("tab") || "conn";
  const timers = [];
  ctx.onCleanup(() => timers.forEach(clearTimeout));

  ctx.root.append(
    pageHead(
      d.name,
      h("span", {}, statusEl, h("span", { class: "ms-3" }, `${d.engine} ${d.version}`), h("span", { class: "ms-3 mono" }, d.domain), h("span", { class: "ms-3" }, "owner: " + d.owner)),
      btn("Backup now", "btn-primary", () => backupNow(), "cloud-arrow-up"),
    ),
    tabsEl,
    pane,
  );

  function showTabs() {
    tabsEl.replaceChildren(
      ...TABS.map(([id, label]) =>
        h("li", { class: "nav-item", role: "presentation" }, h("button", { type: "button", role: "tab", class: "nav-link" + (id === active ? " active" : ""), "aria-selected": String(id === active), onclick: () => select(id) }, label)),
      ),
    );
  }
  async function select(id) {
    active = id;
    timers.splice(0).forEach(clearTimeout);
    showTabs();
    pane.replaceChildren();
    try {
      await { conn, backups, schedules, members }[id]();
    } catch (e) {
      if (ctx.alive()) errToast(e);
    }
  }

  // ---- Kết nối ----
  async function conn() {
    const masked = d.connection_string;
    const value = h("span", { class: "value" }, masked);
    let shown = false;
    let real = "";
    const eye = h("button", { type: "button", class: "btn btn-sm btn-outline-secondary", title: "Hiện mật khẩu (ghi audit)", "aria-label": "Hiện mật khẩu" }, icon("eye"));
    const reveal = async () => {
      if (!real) real = (await api("GET", `/api/databases/${encodeURIComponent(d.name)}/connection`)).connection_string;
      return real;
    };
    eye.addEventListener("click", async () => {
      try {
        shown = !shown;
        value.textContent = shown ? await reveal() : masked;
        eye.replaceChildren(icon(shown ? "eye-slash" : "eye"));
        eye.title = shown ? "Ẩn mật khẩu" : "Hiện mật khẩu (ghi audit)";
      } catch (e) {
        shown = false;
        errToast(e);
      }
    });
    const copy = h("button", { type: "button", class: "btn btn-sm btn-outline-secondary", title: "Sao chép chuỗi kết nối đầy đủ", "aria-label": "Sao chép chuỗi kết nối" }, icon("clipboard"));
    copy.addEventListener("click", async () => {
      try {
        copyText(await reveal());
      } catch (e) {
        errToast(e);
      }
    });
    const app = h(
      "div",
      { class: "panel mb-3" },
      h(
        "div",
        { class: "panel-body" },
        h("h2", { class: "panel-title" }, "User ứng dụng"),
        h("div", { class: "strip mb-3" }, value, eye, copy),
        h("dl", { class: "kv" }, h("dt", {}, "Host"), h("dd", { class: "mono" }, d.domain), h("dt", {}, "Port"), h("dd", { class: "mono" }, String(port)), h("dt", {}, "User"), h("dd", { class: "mono" }, d.user), h("dt", {}, "Database"), h("dd", { class: "mono" }, d.name)),
        h(
          "p",
          { class: "hint mt-3 mb-0" },
          "Kết nối bằng TLS; tên host chọn database (SNI). Muốn kiểm tra chứng chỉ, ",
          h("a", { href: "/api/ca", download: "fridge-ca.crt" }, "tải CA của Fridge"),
          " và dùng sslmode=verify-full.",
        ),
      ),
    );
    pane.append(app);
    if (isOwner && (d.admin_user || d.legacy)) pane.append(superuserPanel());
  }

  function superuserPanel() {
    const box = h("div", { class: "panel-body" }, h("h2", { class: "panel-title" }, "Superuser", d.legacy ? h("span", { class: "badge-soft badge-warn ms-2" }, "kiểu cũ") : null));
    if (d.legacy) {
      box.append(h("p", { class: "mb-0" }, "Database này tạo theo kiểu cũ nên chỉ có một user (user ứng dụng) và không có superuser riêng."));
      return h("div", { class: "panel" }, box);
    }
    const out = h("div", { class: "mt-3" });
    const show = btn("Hiện mật khẩu", "btn-outline-primary btn-sm", async () => {
      try {
        const r = await api("POST", `/api/databases/${encodeURIComponent(d.name)}/superuser/reveal`);
        out.replaceChildren(h("div", { class: "mb-2" }, strip(r.user)), strip(r.password));
      } catch (e) {
        errToast(e);
      }
    });
    const rotate = btn("Rotate…", "btn-outline-danger btn-sm", async () => {
      const ok = await confirmDialog({ title: "Đổi mật khẩu superuser?", body: "Mật khẩu cũ của superuser sẽ không dùng được nữa. User ứng dụng không bị ảnh hưởng.", okLabel: "Rotate", danger: true });
      if (!ok) return;
      try {
        await api("POST", `/api/databases/${encodeURIComponent(d.name)}/superuser/rotate`);
        out.replaceChildren();
        toast("Đã đổi mật khẩu superuser");
      } catch (e) {
        errToast(e);
      }
    });
    box.append(h("p", { class: "mb-2" }, "Dùng để quản trị cả node (extension, dump đầy đủ). User: ", h("span", { class: "mono" }, d.admin_user)), h("div", { class: "actions" }, show, rotate), out);
    return h("div", { class: "panel" }, box);
  }

  // ---- Backups ----
  async function backups() {
    const holder = h("div", {});
    pane.append(holder);
    const load = async () => {
      const rows = await api("GET", `/api/databases/${encodeURIComponent(d.name)}/backups`);
      if (!ctx.alive() || active !== "backups") return;
      holder.replaceChildren(
        table(
          [
            { title: "Thời gian", cell: (b) => fmtTime(b.started_at), cls: "nowrap" },
            { title: "Nguồn", cell: (b) => (b.trigger === "schedule" ? "lịch" : "thủ công") },
            { title: "Destination", cell: (b) => destName(b.destination_id) },
            { title: "Dung lượng", cell: (b) => fmtBytes(b.size_bytes) },
            { title: "Trạng thái", cell: (b) => status(b.status, { pulse: true }) },
            {
              title: "",
              cls: "nowrap text-end",
              cell: (b) =>
                h(
                  "div",
                  { class: "actions justify-content-end" },
                  b.status === "ok" ? btn("Tải", "btn-sm btn-outline-secondary", () => download(`/api/backups/${b.id}/download`, b.s3_key.split("/").pop()).catch(errToast), "download") : null,
                  b.status === "ok" ? btn("Restore…", "btn-sm btn-outline-secondary", () => ctx.navigate(`#/restore?dest=${b.destination_id}&key=${encodeURIComponent(b.s3_key)}`)) : null,
                  btn("Log", "btn-sm btn-outline-secondary", () => logDialog(b)),
                ),
            },
          ],
          rows,
          empty("archive", "Chưa có bản backup nào"),
        ),
      );
      if (rows.some((b) => b.status === "running")) timers.push(setTimeout(() => load().catch(() => {}), 2500));
      else if (d.status !== "running") refreshHeader();
    };
    await load();
  }

  async function refreshHeader() {
    d = await api("GET", "/api/databases/" + encodeURIComponent(name));
    renderStatus();
  }

  function logDialog(b) {
    const text = [b.log, b.error ? "\nLỗi: " + b.error : ""].join("").trim() || "(không có log)";
    modal({ title: `Backup #${b.id}`, size: "modal-lg", body: h("div", {}, h("p", { class: "hint" }, b.s3_key), h("pre", { class: "log mb-0" }, text)) });
  }

  async function backupNow() {
    const sel = h("select", { class: "form-select", name: "destination" }, h("option", { value: "0" }, "Mặc định (theo lịch, hoặc destination của chủ database)"), dests.map((x) => h("option", { value: String(x.id) }, x.name + (x.shared ? " (dùng chung)" : ""))));
    const err = errorBox();
    const go = h("button", { type: "button", class: "btn btn-primary" }, "Bắt đầu backup");
    const m = modal({ title: "Backup now", body: h("div", {}, field("Lưu vào", sel), err.el), footer: [h("button", { type: "button", class: "btn btn-outline-secondary", "data-bs-dismiss": "modal" }, "Hủy"), go] });
    go.addEventListener("click", () =>
      withBusy(go, async () => {
        try {
          const dest = Number(sel.value);
          await api("POST", `/api/databases/${encodeURIComponent(d.name)}/backups`, dest ? { destination_id: dest } : {});
          m.hide();
          toast("Đã bắt đầu backup");
          await select("backups");
        } catch (e) {
          err.set(e.message === "invalid: no usable destination" ? "Chưa có destination nào dùng được — hãy chọn một destination hoặc tạo mới ở trang Destinations." : e.message);
        }
      }),
    );
  }

  // ---- Lịch backup ----
  async function schedules() {
    const holder = h("div", {});
    pane.append(holder);
    const load = async () => {
      const rows = await api("GET", `/api/databases/${encodeURIComponent(d.name)}/schedules`);
      holder.replaceChildren(
        h("div", { class: "mb-3" }, btn("Thêm lịch", "btn-outline-primary btn-sm", () => scheduleDialog(null, load), "plus-lg")),
        table(
          [
            { title: "Destination", cell: (s) => destName(s.destination_id) },
            { title: "Cron", cell: (s) => h("span", { class: "mono" }, s.cron) },
            { title: "Prefix", cell: (s) => s.prefix },
            { title: "Giữ", cell: (s) => String(s.keep_latest) },
            { title: "Bật", cell: (s) => (s.enabled ? "có" : "tắt") },
            {
              title: "",
              cls: "nowrap text-end",
              cell: (s) =>
                h(
                  "div",
                  { class: "actions justify-content-end" },
                  btn("Sửa", "btn-sm btn-outline-secondary", () => scheduleDialog(s, load)),
                  btn("Xóa", "btn-sm btn-outline-danger", async () => {
                    if (!(await confirmDialog({ title: "Xóa lịch backup?", body: "Các bản backup đã có không bị xóa.", okLabel: "Xóa", danger: true }))) return;
                    try {
                      await api("DELETE", "/api/schedules/" + s.id);
                      await load();
                    } catch (e) {
                      errToast(e);
                    }
                  }),
                ),
            },
          ],
          rows,
          empty("calendar-event", "Chưa có lịch backup nào"),
        ),
      );
    };
    await load();
  }

  function scheduleDialog(s, done) {
    const dest = h("select", { class: "form-select", name: "destination" }, dests.map((x) => h("option", { value: String(x.id), selected: s && s.destination_id === x.id }, x.name + (x.shared ? " (dùng chung)" : ""))));
    const cron = input({ name: "cron", value: s ? s.cron : "0 2 * * *" });
    const prefix = input({ name: "prefix", value: s ? s.prefix : "fridge" });
    const keep = input({ name: "keep", type: "number", min: "1", value: String(s ? s.keep_latest : 7) });
    const on = h("input", { class: "form-check-input", type: "checkbox", id: "sched-on", checked: s ? s.enabled : true });
    const err = errorBox();
    const go = h("button", { type: "button", class: "btn btn-primary" }, "Lưu");
    const m = modal({
      title: s ? "Sửa lịch backup" : "Thêm lịch backup",
      body: h("div", {}, field("Destination", dest), field("Cron", cron, "5 trường: phút giờ ngày tháng thứ. Múi giờ theo máy chủ."), field("Prefix", prefix), field("Số bản giữ lại", keep), h("div", { class: "form-check" }, on, h("label", { class: "form-check-label", for: "sched-on" }, "Bật lịch này")), err.el),
      footer: [h("button", { type: "button", class: "btn btn-outline-secondary", "data-bs-dismiss": "modal" }, "Hủy"), go],
    });
    go.addEventListener("click", () =>
      withBusy(go, async () => {
        const body = { destination_id: Number(dest.value), cron: cron.value.trim(), prefix: prefix.value.trim(), keep_latest: Number(keep.value), enabled: on.checked };
        try {
          if (s) await api("PUT", "/api/schedules/" + s.id, body);
          else await api("POST", `/api/databases/${encodeURIComponent(d.name)}/schedules`, body);
          m.hide();
          await done();
        } catch (e) {
          err.set(e.message === "destination not usable" ? "Destination này không dùng được cho database này." : e.message);
        }
      }),
    );
  }

  // ---- Người quản lý ----
  async function members() {
    const holder = h("div", {});
    pane.append(holder);
    const load = async () => {
      const rows = await api("GET", `/api/databases/${encodeURIComponent(d.name)}/members`);
      const user = input({ name: "member", placeholder: "tên đăng nhập" });
      const err = errorBox();
      const add = h("form", { class: "d-flex gap-2 mb-1", novalidate: true }, user, h("button", { type: "submit", class: "btn btn-outline-primary text-nowrap" }, "Thêm"));
      add.addEventListener("submit", async (e) => {
        e.preventDefault();
        err.set("");
        try {
          await api("POST", `/api/databases/${encodeURIComponent(d.name)}/members`, { username: user.value.trim() });
          await load();
        } catch (e2) {
          err.set(e2.message);
        }
      });
      holder.replaceChildren(
        h("p", { class: "hint" }, "Người quản lý xem được chuỗi kết nối, backup và đặt lịch cho database này, nhưng không thêm hay gỡ người khác."),
        isOwner ? h("div", { class: "mb-3" }, add, err.el) : null,
        table(
          [
            { title: "Tên đăng nhập", cell: (m) => m.username },
            { title: "Từ", cell: (m) => ago(m.added_at) },
            {
              title: "",
              cls: "text-end",
              cell: (m) =>
                isOwner
                  ? btn("Gỡ", "btn-sm btn-outline-danger", async () => {
                      try {
                        await api("DELETE", `/api/databases/${encodeURIComponent(d.name)}/members/${m.user_id}`);
                        await load();
                      } catch (e) {
                        errToast(e);
                      }
                    })
                  : "",
            },
          ],
          rows,
          empty("people", "Chưa có người quản lý nào"),
        ),
      );
    };
    await load();
  }

  await select(["conn", "backups", "schedules", "members"].includes(active) ? active : "conn");
  if (d.status === "creating") {
    const poll = async () => {
      await refreshHeader().catch(() => {});
      if (ctx.alive() && d.status === "creating") timers.push(setTimeout(poll, 2500));
    };
    timers.push(setTimeout(poll, 2500));
  }
}
