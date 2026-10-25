import { api } from "../api.js";
import { h, pageHead, btn, table, empty, modal, field, input, errorBox, withBusy, errToast, toast, confirmDialog } from "../ui.js";

export default async function destinations(ctx) {
  const me = ctx.me;
  const body = h("div", {});
  ctx.root.append(pageHead("Destinations", "Nơi lưu backup (S3 hoặc tương thích)", btn("Thêm destination", "btn-primary", () => dialog(null), "plus-lg")), body);
  const manages = (d) => me.role === "admin" || d.owner === me.username;

  async function load() {
    const rows = await api("GET", "/api/destinations");
    const cols = [
      { title: "Tên", cell: (d) => h("span", { class: "fw-semibold" }, d.name) },
      { title: "Endpoint", cell: (d) => (d.endpoint ? h("span", { class: "mono trunc d-inline-block" }, d.endpoint) : h("span", { class: "hint" }, "dùng chung")) },
      { title: "Bucket", cell: (d) => d.bucket || "" },
    ];
    if (me.role === "admin") {
      cols.push({ title: "Owner", cell: (d) => d.owner });
      cols.push({
        title: "Dùng chung",
        cell: (d) => {
          const sw = h("input", { class: "form-check-input", type: "checkbox", role: "switch", checked: d.shared, "aria-label": "Dùng chung " + d.name });
          sw.addEventListener("change", async () => {
            try {
              await api("PATCH", "/api/destinations/" + d.id, { shared: sw.checked });
              toast(sw.checked ? "Đã bật dùng chung" : "Đã tắt dùng chung");
            } catch (e) {
              sw.checked = !sw.checked;
              errToast(e);
            }
          });
          return h("div", { class: "form-check form-switch" }, sw);
        },
      });
    } else cols.push({ title: "Dùng chung", cell: (d) => (d.shared ? "có" : "") });
    cols.push({
      title: "",
      cls: "nowrap text-end",
      cell: (d) =>
        manages(d)
          ? h(
              "div",
              { class: "actions justify-content-end" },
              btn("Test", "btn-sm btn-outline-secondary", () => test(d)),
              btn("Sửa", "btn-sm btn-outline-secondary", () => dialog(d)),
              btn("Xóa", "btn-sm btn-outline-danger", () => remove(d)),
            )
          : "",
    });
    body.replaceChildren(table(cols, rows, empty("cloud", "Chưa có destination nào", btn("Thêm destination", "btn-primary btn-sm", () => dialog(null)))));
  }

  async function test(d) {
    try {
      await api("POST", `/api/destinations/${d.id}/test`);
      toast(`${d.name}: kết nối được`);
    } catch (e) {
      errToast(new Error(`${d.name}: ${e.message}`));
    }
  }

  async function remove(d) {
    if (!(await confirmDialog({ title: `Xóa destination ${d.name}?`, body: "Các bản backup đã lưu trong bucket không bị xóa. Không xóa được nếu còn lịch backup dùng destination này.", okLabel: "Xóa", danger: true }))) return;
    try {
      await api("DELETE", "/api/destinations/" + d.id);
      await load();
    } catch (e) {
      errToast(e);
    }
  }

  function dialog(d) {
    const f = {
      name: input({ name: "name", value: d?.name || "" }),
      endpoint: input({ name: "endpoint", placeholder: "https://s3.example.com", value: d?.endpoint || "" }),
      region: input({ name: "region", placeholder: "tùy chọn", value: d?.region || "" }),
      bucket: input({ name: "bucket", value: d?.bucket || "" }),
      access: input({ name: "access_key", value: d?.access_key || "" }),
      secret: input({ name: "secret_key", type: "password", autocomplete: "new-password", placeholder: d ? "để trống = giữ nguyên" : "" }),
    };
    const path = h("input", { class: "form-check-input", type: "checkbox", id: "dest-path", checked: d ? d.path_style : false });
    const epErr = errorBox();
    const err = errorBox();
    const go = h("button", { type: "button", class: "btn btn-primary" }, "Lưu");
    const m = modal({
      title: d ? `Sửa ${d.name}` : "Thêm destination",
      body: h(
        "form",
        { novalidate: true },
        field("Tên", f.name),
        h("div", { class: "mb-3" }, h("label", { class: "form-label", for: "dest-ep" }, "Endpoint"), Object.assign(f.endpoint, { id: "dest-ep" }), epErr.el),
        h("div", { class: "row" }, h("div", { class: "col-7" }, field("Bucket", f.bucket)), h("div", { class: "col-5" }, field("Region", f.region))),
        field("Access key", f.access),
        field("Secret key", f.secret),
        h("div", { class: "form-check" }, path, h("label", { class: "form-check-label", for: "dest-path" }, "Path-style (MinIO và nhiều S3 tự dựng cần bật)")),
        err.el,
      ),
      footer: [h("button", { type: "button", class: "btn btn-outline-secondary", "data-bs-dismiss": "modal" }, "Hủy"), go],
    });
    go.addEventListener("click", () =>
      withBusy(go, async () => {
        epErr.set("");
        err.set("");
        if (!f.name.value.trim() || !f.endpoint.value.trim() || !f.bucket.value.trim() || !f.access.value.trim() || (!d && !f.secret.value)) return err.set("Điền tên, endpoint, bucket, access key và secret key");
        const b = { name: f.name.value.trim(), endpoint: f.endpoint.value.trim(), region: f.region.value.trim(), bucket: f.bucket.value.trim(), access_key: f.access.value.trim(), secret_key: f.secret.value, path_style: path.checked };
        try {
          if (d) await api("PUT", "/api/destinations/" + d.id, b);
          else await api("POST", "/api/destinations", b);
          m.hide();
          toast("Đã lưu");
          await load();
        } catch (e) {
          if (/endpoint/i.test(e.message)) epErr.set(e.message);
          else err.set(e.message);
        }
      }),
    );
  }

  await load();
}
