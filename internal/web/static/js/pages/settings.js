import { api } from "../api.js";
import { h, icon, pageHead, btn, table, empty, fmtTime, ago, errToast, toast, confirmDialog } from "../ui.js";

// A textarea holding one value per line, saved with PUT.
function listEditor({ title, help, load, save }) {
  const ta = h("textarea", { class: "form-control mono", rows: "4", spellcheck: "false", "aria-label": title });
  const err = h("div", { class: "err-text", role: "alert" });
  const go = btn("Lưu", "btn-primary btn-sm", async () => {
    err.textContent = "";
    try {
      const lines = ta.value.split(/\s+/).filter(Boolean);
      const saved = await save(lines);
      ta.value = saved.join("\n");
      toast("Đã lưu");
    } catch (e) {
      err.textContent = e.message;
    }
  });
  load().then((v) => (ta.value = v.join("\n"))).catch(errToast);
  return h("div", { class: "panel mb-3" }, h("div", { class: "panel-body" }, h("h2", { class: "panel-title" }, title), h("p", { class: "hint" }, help), ta, err, h("div", { class: "mt-2" }, go)));
}

export default async function settings(ctx) {
  ctx.root.append(pageHead("Settings"));
  if (ctx.me.role === "admin") {
    ctx.root.append(
      listEditor({
        title: "Allowlist IP",
        help: "Chỉ các dải CIDR này được truy cập database và trang quản lý qua Traefik, mỗi dòng một dải. Đừng tự khóa mình: nếu lỡ tay, chạy fridge recover allowlist trong container.",
        load: async () => (await api("GET", "/api/settings/allowlist")).cidrs,
        save: async (cidrs) => (await api("PUT", "/api/settings/allowlist", { cidrs })).cidrs,
      }),
      listEditor({
        title: "Endpoint S3 được miễn kiểm tra",
        help: "Destination của người dùng thường không được trỏ vào mạng nội bộ của Fridge, loopback hay link-local. Host (không kèm cổng) nằm trong danh sách này được bỏ qua kiểm tra đó, mỗi dòng một host.",
        load: async () => (await api("GET", "/api/settings/endpoint-allowlist")).hosts,
        save: async (hosts) => (await api("PUT", "/api/settings/endpoint-allowlist", { hosts })).hosts,
      }),
    );
  }
  ctx.root.append(
    h(
      "div",
      { class: "panel mb-3" },
      h("div", { class: "panel-body" }, h("h2", { class: "panel-title" }, "Chứng chỉ CA"), h("p", { class: "hint" }, "Thêm CA này vào máy bạn để trình duyệt và client database tin chứng chỉ của Fridge."), h("a", { class: "btn btn-outline-primary btn-sm", href: "/api/ca", download: "fridge-ca.crt" }, icon("download"), " Tải CA")),
    ),
  );

  const holder = h("div", {});
  ctx.root.append(h("h2", { class: "fs-5 mt-4 mb-2" }, "Phiên đăng nhập của tôi"), holder);
  async function sessions() {
    const rows = await api("GET", "/api/me/sessions");
    holder.replaceChildren(
      table(
        [
          { title: "Tạo lúc", cell: (s) => fmtTime(s.created_at), cls: "nowrap" },
          { title: "Hoạt động cuối", cell: (s) => ago(s.last_seen_at), cls: "nowrap" },
          { title: "IP", cell: (s) => h("span", { class: "mono" }, s.ip) },
          { title: "Trình duyệt", cell: (s) => h("span", { class: "trunc d-inline-block", title: s.user_agent }, s.user_agent || "—") },
          {
            title: "",
            cls: "text-end",
            cell: (s) =>
              s.current
                ? h("span", { class: "badge-soft" }, "phiên hiện tại")
                : btn("Thu hồi", "btn-sm btn-outline-danger", async () => {
                    if (!(await confirmDialog({ title: "Thu hồi phiên này?", body: "Thiết bị đang dùng phiên đó sẽ bị đăng xuất ngay.", okLabel: "Thu hồi", danger: true }))) return;
                    try {
                      await api("DELETE", "/api/sessions/" + s.id);
                      await sessions();
                    } catch (e) {
                      errToast(e);
                    }
                  }),
          },
        ],
        rows,
        empty("shield-lock", "Không có phiên nào"),
      ),
    );
  }
  await sessions();
}
