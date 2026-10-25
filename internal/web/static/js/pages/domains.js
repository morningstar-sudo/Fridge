import { api } from "../api.js";
import { h, pageHead, btn, table, empty, input, errorBox, errToast, toast, confirmDialog } from "../ui.js";

export default async function domains(ctx) {
  const me = ctx.me;
  const name = input({ name: "domain", placeholder: "vd: shop.example.com" });
  const err = errorBox();
  const add = h("form", { class: "d-flex gap-2", novalidate: true }, name, h("button", { type: "submit", class: "btn btn-primary text-nowrap" }, "Đăng ký domain"));
  const body = h("div", {});
  ctx.root.append(pageHead("Domains", "Mỗi database cần một domain đã đăng ký; client chọn database theo tên này (TLS SNI)."), h("div", { class: "panel mb-3 narrow-wide" }, h("div", { class: "panel-body" }, add, err.el)), body);

  add.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.set("");
    try {
      await api("POST", "/api/domains", { name: name.value.trim() });
      name.value = "";
      toast("Đã đăng ký domain");
      await load();
    } catch (e2) {
      err.set(e2.message);
    }
  });

  async function load() {
    const rows = await api("GET", "/api/domains");
    const cols = [
      { title: "Domain", cell: (d) => h("span", { class: "mono fw-semibold" }, d.name) },
      { title: "Database đang dùng", cell: (d) => String(d.in_use) },
    ];
    if (me.role === "admin") cols.push({ title: "Owner", cell: (d) => d.owner });
    cols.push({
      title: "",
      cls: "text-end",
      cell: (d) => {
        const b = btn("Xóa", "btn-sm btn-outline-danger", async () => {
          if (!(await confirmDialog({ title: `Xóa domain ${d.name}?`, body: "Domain được trả về để người khác đăng ký.", okLabel: "Xóa", danger: true }))) return;
          try {
            await api("DELETE", "/api/domains/" + d.id);
            await load();
          } catch (e) {
            errToast(e);
          }
        });
        if (d.in_use > 0) {
          b.disabled = true;
          b.title = "Đang có database dùng domain này";
        }
        return b;
      },
    });
    body.replaceChildren(table(cols, rows, empty("globe2", "Chưa đăng ký domain nào")));
  }
  await load();
}
