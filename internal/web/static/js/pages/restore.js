import { api } from "../api.js";
import { h, pageHead, btn, table, empty, status, fmtTime, fmtBytes, field, input, errorBox, withBusy, errToast } from "../ui.js";
import { credentialFields } from "./creds.js";

// The version in a key like "fridge/postgres-17/db/db-2026….dump", as a hint for the new node.
const sourceVersion = (key) => /(?:^|\/)postgres-(\d+)(?:\/|$)/.exec(key)?.[1] || "";

export default async function restore(ctx) {
  const timers = [];
  ctx.onCleanup(() => timers.forEach(clearTimeout));
  const [dests, engines, domains] = await Promise.all([api("GET", "/api/destinations"), api("GET", "/api/engines"), api("GET", "/api/domains")]);
  const mine = domains.filter((d) => d.owner === ctx.me.username);
  const versions = engines.find((e) => e.name === "postgres")?.versions || [];

  const destSel = h("select", { class: "form-select", name: "destination" }, h("option", { value: "" }, "Chọn destination…"), dests.map((d) => h("option", { value: String(d.id) }, d.name + (d.shared ? " (dùng chung)" : ""))));
  const q = input({ name: "q", placeholder: "Tìm theo tên file" });
  const list = h("div", {});
  const detail = h("div", { class: "mt-3" });
  let chosen = null; // {dest, key}

  ctx.root.append(
    pageHead("Restore", "Khôi phục một bản backup vào node database mới"),
    h("div", { class: "panel mb-3" }, h("div", { class: "panel-body" }, h("div", { class: "row g-3" }, h("div", { class: "col-md-5" }, field("Destination", destSel)), h("div", { class: "col-md-7" }, field("Lọc file", q))))),
    list,
    detail,
  );

  async function loadFiles() {
    detail.replaceChildren();
    if (!destSel.value) return list.replaceChildren(h("div", { class: "panel" }, empty("cloud-arrow-down", "Chọn destination để xem các file backup")));
    try {
      const r = await api("GET", `/api/destinations/${destSel.value}/objects?` + new URLSearchParams({ q: q.value }));
      list.replaceChildren(
        table(
          [
            { title: "File", cell: (o) => h("span", { class: "mono" }, o.key) },
            { title: "Database", cell: (o) => o.database || h("span", { class: "hint" }, "(file tải lên)") },
            { title: "Dung lượng", cell: (o) => fmtBytes(o.size) },
            { title: "Cập nhật", cell: (o) => fmtTime(o.last_modified), cls: "nowrap" },
            { title: "", cls: "text-end", cell: (o) => btn("Chọn", "btn-sm btn-outline-primary", () => pick(o.key)) },
          ],
          r.objects || [],
          empty("archive", "Không có file nào phù hợp"),
        ),
        r.truncated ? h("p", { class: "hint mt-2" }, "Chỉ hiện một phần kết quả — hãy lọc theo tên để thu hẹp.") : null,
      );
    } catch (e) {
      list.replaceChildren();
      errToast(e);
    }
  }
  destSel.addEventListener("change", loadFiles);
  let qt;
  q.addEventListener("input", () => {
    clearTimeout(qt);
    qt = setTimeout(loadFiles, 300);
  });
  ctx.onCleanup(() => clearTimeout(qt));

  function pick(key) {
    chosen = { dest: Number(destSel.value), key };
    const creds = credentialFields(ctx.me.username);
    const ver = h("select", { class: "form-select", name: "version" }, h("option", { value: "" }, "Theo version của file"), versions.map((v) => h("option", { value: v, selected: v === sourceVersion(key) }, v)));
    const dom = h("select", { class: "form-select", name: "domain" }, mine.map((d) => h("option", { value: d.name }, d.name)));
    const err = errorBox();
    const go = h("button", { type: "submit", class: "btn btn-primary" }, "Restore");
    const checkBtn = btn("Kiểm tra kỹ", "btn-outline-secondary", () => deepCheck(checkOut, checkBtn), "shield-check");
    const checkOut = h("div", { class: "mt-3" });
    const status_ = h("div", { class: "mt-3" });
    const form = h(
      "form",
      { novalidate: true },
      h("p", { class: "mb-3" }, "File: ", h("span", { class: "mono" }, key)),
      h("div", { class: "row" }, h("div", { class: "col-md-6" }, creds.nodes[0]), h("div", { class: "col-md-3" }, field("Domain", dom)), h("div", { class: "col-md-3" }, field("Version", ver))),
      mine.length ? null : h("div", { class: "alert alert-warning" }, "Bạn chưa đăng ký domain nào. ", h("a", { href: "#/domains" }, "Đăng ký domain"), " trước."),
      creds.nodes.slice(1),
      err.el,
      h("div", { class: "actions mt-3" }, go, checkBtn),
      checkOut,
      status_,
    );
    detail.replaceChildren(h("div", { class: "panel" }, h("div", { class: "panel-body" }, h("h2", { class: "panel-title" }, "Restore vào node mới"), form)));
    detail.scrollIntoView({ behavior: "smooth", block: "nearest" });
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      err.set("");
      const bad = creds.check();
      if (bad) return err.set(bad);
      if (!dom.value) return err.set("Chọn một domain");
      await withBusy(go, async () => {
        const v = creds.values();
        const nw = { name: v.name, domain: dom.value, version: ver.value };
        if (v.db_user) nw.db_user = v.db_user;
        if (v.db_password) nw.db_password = v.db_password;
        try {
          const r = await api("POST", "/api/restores", { destination_id: chosen.dest, key: chosen.key, new: nw });
          watchRestore(status_, r.id);
        } catch (e2) {
          err.set(e2.message.replace(/^invalid: /, ""));
        }
      });
    });
  }

  async function deepCheck(out, button) {
    await withBusy(button, async () => {
      try {
        const c = await api("POST", "/api/dump-checks", { destination_id: chosen.dest, key: chosen.key });
        out.replaceChildren(h("div", {}, status("running", { pulse: true }), " đang kiểm tra bằng pg_restore…"));
        const poll = async () => {
          const r = await api("GET", "/api/dump-checks/" + c.id);
          if (!ctx.alive()) return;
          if (r.status === "running") return timers.push(setTimeout(() => poll().catch(errToast), 2500));
          const res = r.result || {};
          out.replaceChildren(
            h(
              "div",
              { class: "panel" },
              h(
                "div",
                { class: "panel-body" },
                h("div", { class: "mb-2" }, status(r.status), r.status === "ok" ? " File đọc được trọn vẹn." : " " + (r.error || "")),
                h(
                  "dl",
                  { class: "kv" },
                  res.source_version ? [h("dt", {}, "Server nguồn"), h("dd", {}, res.source_version), h("dt", {}, "pg_dump"), h("dd", {}, res.pg_dump_version)] : null,
                  res.toc_entries != null ? [h("dt", {}, "Số mục"), h("dd", {}, String(res.toc_entries))] : null,
                  res.bytes ? [h("dt", {}, "Dung lượng"), h("dd", {}, fmtBytes(res.bytes))] : null,
                ),
                r.log ? h("pre", { class: "log mt-2 mb-0" }, r.log) : null,
              ),
            ),
          );
        };
        await poll();
      } catch (e) {
        out.replaceChildren(h("p", { class: "err-text" }, e.message));
      }
    });
  }

  function watchRestore(out, id) {
    out.replaceChildren(h("div", {}, status("running", { pulse: true }), " đang restore…"));
    const poll = async () => {
      const r = await api("GET", "/api/restores/" + id);
      if (!ctx.alive()) return;
      if (r.status === "running") return timers.push(setTimeout(() => poll().catch(errToast), 2500));
      if (r.status === "ok" && r.target_database_id) {
        const d = await api("GET", "/api/databases/" + r.target_database_id);
        out.replaceChildren(h("div", {}, status("ok"), " Restore xong. ", h("a", { href: "#/databases/" + encodeURIComponent(d.name) }, "Mở " + d.name)));
      } else {
        out.replaceChildren(h("div", {}, status("failed"), " " + (r.error || "Restore lỗi")), r.log ? h("pre", { class: "log mt-2 mb-0" }, r.log) : null);
      }
    };
    poll().catch(errToast);
  }

  const wantDest = ctx.query.get("dest");
  if (wantDest) {
    destSel.value = wantDest;
    await loadFiles();
    if (ctx.query.get("key")) pick(ctx.query.get("key"));
  } else await loadFiles();
}
