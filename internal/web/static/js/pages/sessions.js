import { api } from "../api.js";
import { h, pageHead, btn, table, empty, fmtTime, ago, errToast, toast, confirmDialog } from "../ui.js";

export default async function sessions(ctx) {
  const body = h("div", {});
  ctx.root.append(pageHead("Sessions", "Mọi phiên đăng nhập đang còn hiệu lực"), body);
  async function load() {
    const rows = await api("GET", "/api/sessions");
    body.replaceChildren(
      table(
        [
          { title: "Người dùng", cell: (s) => h("span", { class: "fw-semibold" }, s.username) },
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
                    if (!(await confirmDialog({ title: `Thu hồi phiên của ${s.username}?`, body: "Họ bị đăng xuất ngay.", okLabel: "Thu hồi", danger: true }))) return;
                    try {
                      await api("DELETE", "/api/sessions/" + s.id);
                      toast("Đã thu hồi");
                      await load();
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
  await load();
}
