import { api } from "../api.js";
import { h, pageHead, btn, table, empty, modal, fmtBytes, ago, fmtTime, errToast, toast, withBusy } from "../ui.js";

// "18.4-1.pgdg13+1" is the Debian package suffix; people mean 18.4.
const short = (v) => (v || "").split("-")[0];

export default async function images(ctx) {
  const body = h("div", {});
  const progress = h("div", { class: "mb-3" });
  const timers = [];
  ctx.onCleanup(() => timers.forEach(clearTimeout));
  const checkBtn = btn("Kiểm tra cập nhật", "btn-outline-secondary", checkUpdates, "arrow-repeat");
  ctx.root.append(
    pageHead("Images", "Docker image dùng để tạo node database", btn("Pull…", "btn-outline-primary", pullDialog, "cloud-download"), btn("Pull tất cả", "btn-outline-primary", () => pull(null), "cloud-download"), checkBtn, btn("Prune…", "btn-outline-danger", pruneDialog, "trash3")),
    progress,
    body,
  );
  let rows = [];
  let catalog = [];

  function badge(r) {
    const u = r.update;
    if (!u) return h("span", { class: "hint" }, "chưa kiểm tra");
    if (u.error) return h("span", { class: "hint", title: u.error }, "không kiểm tra được");
    if (u.available) return h("span", { class: "badge-soft badge-warn" }, `New version: ${short(u.local_version) || "?"} → ${short(u.remote_version) || "?"}`);
    return h("span", { class: "hint" }, "mới nhất");
  }

  function draw() {
    body.replaceChildren(
      table(
        [
          { title: "Image", cell: (r) => h("span", { class: "mono fw-semibold" }, r.ref) },
          { title: "Trạng thái", cell: (r) => (r.present ? h("span", {}, "đã có ", h("span", { class: "hint mono" }, r.short_digest)) : h("span", { class: "hint" }, "chưa pull")) },
          { title: "Version", cell: (r) => short(r.version) || "—" },
          { title: "Dung lượng", cell: (r) => fmtBytes(r.size) },
          { title: "Tạo", cell: (r) => (r.created ? fmtTime(r.created).slice(0, 10) : "—"), cls: "nowrap" },
          { title: "Đang dùng", cell: (r) => (r.used_by.length ? h("span", { title: r.used_by.join(", ") }, `${r.used_by.length} database`) : h("span", { class: "hint" }, "không")) },
          { title: "Dùng lần cuối", cell: (r) => (r.last_used ? ago(r.last_used) : "—") },
          { title: "Cập nhật", cell: badge },
        ],
        rows,
        empty("boxes", "Chưa có image nào"),
      ),
    );
  }

  async function load() {
    rows = await api("GET", "/api/images");
    draw();
  }

  async function checkUpdates() {
    await withBusy(checkBtn, async () => {
      try {
        rows = await api("POST", "/api/images/check-updates");
        draw();
        toast("Đã kiểm tra");
      } catch (e) {
        errToast(e);
      }
    });
  }

  async function pull(refs) {
    const list = refs || catalog;
    if (!list.length) return toast("Chọn ít nhất một image", true);
    try {
      const { job } = await api("POST", "/api/images/pull", { refs: list });
      const poll = async () => {
        const { status } = await api("GET", "/api/images/pull/" + job);
        if (!ctx.alive()) return;
        progress.replaceChildren(
          h(
            "div",
            { class: "panel" },
            h(
              "div",
              { class: "panel-body" },
              Object.entries(status).map(([ref, st]) => h("div", {}, h("span", { class: "mono" }, ref), " — ", st === "pulling" ? h("span", { class: "status running pulse" }, "đang pull") : st === "done" ? h("span", { class: "status ok" }, "xong") : h("span", { class: "status failed" }, st.replace(/^error: /, ""))))
            ),
          ),
        );
        if (Object.values(status).includes("pulling")) timers.push(setTimeout(() => poll().catch(errToast), 2500));
        else await load();
      };
      await poll();
    } catch (e) {
      errToast(e);
    }
  }

  function pullDialog() {
    const boxes = catalog.map((ref) => ({ ref, el: h("input", { class: "form-check-input", type: "checkbox", id: "pull-" + ref }) }));
    const go = h("button", { type: "button", class: "btn btn-primary" }, "Pull");
    const m = modal({
      title: "Pull image",
      body: h("div", {}, boxes.map((b) => h("div", { class: "form-check" }, b.el, h("label", { class: "form-check-label mono", for: "pull-" + b.ref }, b.ref)))),
      footer: [h("button", { type: "button", class: "btn btn-outline-secondary", "data-bs-dismiss": "modal" }, "Hủy"), go],
    });
    go.addEventListener("click", () => {
      m.hide();
      pull(boxes.filter((b) => b.el.checked).map((b) => b.ref));
    });
  }

  async function pruneDialog() {
    try {
      const pv = await api("GET", "/api/images/prune");
      const go = h("button", { type: "button", class: "btn btn-danger" }, `Xóa ${pv.images.length} image`);
      const m = modal({
        title: "Dọn image không dùng",
        body: pv.images.length
          ? h("div", {}, h("p", {}, `Các image dưới đây không có database nào dùng. Xóa chúng giải phóng khoảng ${fmtBytes(pv.bytes)}; pull lại được khi cần.`), h("ul", {}, pv.images.map((i) => h("li", {}, h("span", { class: "mono" }, i.ref), " — " + fmtBytes(i.size)))))
          : h("p", { class: "mb-0" }, "Không có image nào để dọn: image nào đang có cũng có database dùng."),
        footer: pv.images.length ? [h("button", { type: "button", class: "btn btn-outline-secondary", "data-bs-dismiss": "modal" }, "Hủy"), go] : [],
      });
      go.addEventListener("click", () =>
        withBusy(go, async () => {
          try {
            const r = await api("POST", "/api/images/prune", { refs: pv.images.map((i) => i.ref) });
            m.hide();
            toast(`Đã xóa ${r.removed.length} image` + (r.errors ? ` (có lỗi: ${r.errors})` : ""), !!r.errors);
            await load();
          } catch (e) {
            errToast(e);
          }
        }),
      );
    } catch (e) {
      errToast(e);
    }
  }

  const engines = await api("GET", "/api/engines");
  catalog = engines.flatMap((e) => e.versions.map((v) => `${e.name}:${v}`));
  await load();
}
