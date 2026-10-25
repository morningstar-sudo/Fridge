import { api } from "../api.js";
import { h, pageHead, table, empty, ago, fmtTime, elapsed, errToast } from "../ui.js";

const KIND = { backup: "Backup", restore: "Restore", dumpcheck: "Kiểm tra kỹ", image_pull: "Pull image" };

export default async function activity(ctx) {
  const jobs = h("div", {});
  const online = h("div", {});
  ctx.root.append(pageHead("Hoạt động", "Việc đang chạy và người đang online (làm mới mỗi 3 giây)"), h("h2", { class: "fs-5 mb-2" }, "Đang chạy"), jobs, h("h2", { class: "fs-5 mt-4 mb-2" }, "Đang online (5 phút gần đây)"), online);
  let timer;
  ctx.onCleanup(() => clearTimeout(timer));
  async function tick() {
    try {
      const r = await api("GET", "/api/activity");
      if (!ctx.alive()) return;
      jobs.replaceChildren(
        table(
          [
            { title: "Loại", cell: (j) => KIND[j.kind] || j.kind },
            { title: "Đối tượng", cell: (j) => h("span", { class: "mono" }, j.database || "—") },
            { title: "Người bắt đầu", cell: (j) => j.started_by },
            { title: "Bắt đầu lúc", cell: (j) => fmtTime(j.started_at), cls: "nowrap" },
            { title: "Đã chạy", cell: (j) => h("span", { class: "status running pulse" }, elapsed(j.started_at)) },
          ],
          r.jobs,
          empty("moon-stars", "Không có việc nào đang chạy"),
        ),
      );
      online.replaceChildren(
        table(
          [
            { title: "Người dùng", cell: (o) => h("span", { class: "fw-semibold" }, o.username) },
            { title: "IP", cell: (o) => h("span", { class: "mono" }, o.ip) },
            { title: "Hoạt động cuối", cell: (o) => ago(o.last_seen_at) },
            { title: "Trình duyệt", cell: (o) => h("span", { class: "trunc d-inline-block", title: o.user_agent }, o.user_agent || "—") },
          ],
          r.online,
          empty("person", "Không ai online"),
        ),
      );
    } catch (e) {
      if (ctx.alive() && e.status !== 401) errToast(e);
    }
    if (ctx.alive()) timer = setTimeout(tick, 3000);
  }
  await tick();
}
