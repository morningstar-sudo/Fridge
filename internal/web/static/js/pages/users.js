import { api } from "../api.js";
import { h, pageHead, btn, table, empty, fmtTime, modal, field, input, errorBox, withBusy, errToast, toast, strip, confirmDialog, USERNAME_RE, USERNAME_HINT } from "../ui.js";

function linkDialog(title, text, link) {
  modal({ title, body: h("div", {}, h("p", {}, text), strip(link), h("p", { class: "hint mt-2 mb-0" }, "Link dùng được một lần và hết hạn sau 48 giờ. Gửi nó qua kênh riêng tư.")) });
}

export default async function users(ctx) {
  const body = h("div", {});
  ctx.root.append(pageHead("Users", "Tài khoản đăng nhập Fridge", btn("Mời người dùng", "btn-primary", invite, "person-plus")), body);

  async function load() {
    const rows = await api("GET", "/api/users");
    body.replaceChildren(
      table(
        [
          { title: "Tên đăng nhập", cell: (u) => h("span", { class: "fw-semibold" }, u.username) },
          { title: "Vai trò", cell: (u) => (u.role === "admin" ? h("span", { class: "badge-soft" }, "admin") : "user") },
          { title: "Trạng thái", cell: (u) => (u.disabled ? h("span", { class: "status failed" }, "bị khóa") : h("span", { class: "status ok" }, "hoạt động")) },
          { title: "Tạo lúc", cell: (u) => fmtTime(u.created_at), cls: "nowrap" },
          {
            title: "",
            cls: "nowrap text-end",
            cell: (u) =>
              h(
                "div",
                { class: "actions justify-content-end" },
                btn(u.role === "admin" ? "Hạ xuống user" : "Nâng lên admin", "btn-sm btn-outline-secondary", () => patch(u, { role: u.role === "admin" ? "user" : "admin" })),
                btn(u.disabled ? "Mở khóa" : "Khóa", "btn-sm btn-outline-secondary", () => patch(u, { disabled: !u.disabled })),
                btn("Reset mật khẩu", "btn-sm btn-outline-secondary", () => reset(u)),
              ),
          },
        ],
        rows,
        empty("people", "Chưa có người dùng"),
      ),
    );
  }

  async function patch(u, change) {
    if (change.disabled === true && !(await confirmDialog({ title: `Khóa ${u.username}?`, body: "Mọi phiên đăng nhập của tài khoản này kết thúc ngay.", okLabel: "Khóa", danger: true }))) return;
    try {
      await api("PATCH", "/api/users/" + u.id, change);
      toast("Đã cập nhật " + u.username);
    } catch (e) {
      errToast(/last admin/i.test(e.message) ? new Error("Không đổi được: đây là quản trị viên cuối cùng còn hoạt động. Hãy nâng người khác lên admin trước.") : e);
    }
    await load();
  }

  async function reset(u) {
    try {
      const r = await api("POST", `/api/users/${u.id}/reset`);
      linkDialog(`Reset mật khẩu của ${u.username}`, "Gửi link này cho họ để đặt mật khẩu mới. Các phiên đang đăng nhập của họ vẫn còn cho tới khi họ đổi mật khẩu.", r.link);
    } catch (e) {
      errToast(e);
    }
  }

  function invite() {
    const name = input({ name: "invite-username" });
    const role = h("select", { class: "form-select", name: "role" }, h("option", { value: "user" }, "user"), h("option", { value: "admin" }, "admin"));
    const err = errorBox();
    const go = h("button", { type: "button", class: "btn btn-primary" }, "Tạo lời mời");
    const m = modal({ title: "Mời người dùng", body: h("form", { novalidate: true }, field("Tên đăng nhập", name, USERNAME_HINT), field("Vai trò", role), err.el), footer: [h("button", { type: "button", class: "btn btn-outline-secondary", "data-bs-dismiss": "modal" }, "Hủy"), go] });
    go.addEventListener("click", () =>
      withBusy(go, async () => {
        err.set("");
        if (!USERNAME_RE.test(name.value.trim())) return err.set("Tên đăng nhập: " + USERNAME_HINT);
        try {
          const r = await api("POST", "/api/invites", { username: name.value.trim(), role: role.value });
          m.hide();
          linkDialog(`Lời mời cho ${name.value.trim()}`, "Gửi link này cho họ để tạo tài khoản.", r.link);
          await load();
        } catch (e) {
          err.set(e.message);
        }
      }),
    );
  }
  await load();
}
