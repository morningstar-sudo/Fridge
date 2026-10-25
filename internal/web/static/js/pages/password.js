import { api } from "../api.js";
import { makeCredentials, scramLogin } from "../scram.js";
import { h, field, input, passwordPair, errorBox, withBusy, pageHead, toast } from "../ui.js";

// Changing the password needs the old one first (a SCRAM round that yields a one-time token).
export default async function password(ctx) {
  const old = input({ type: "password", name: "current-password", autocomplete: "current-password" });
  const pw = passwordPair("Mật khẩu mới");
  const err = errorBox();
  const submit = h("button", { type: "submit", class: "btn btn-primary" }, "Đổi mật khẩu");
  const form = h("form", { novalidate: true, class: "panel-body" }, field("Mật khẩu hiện tại", old), pw.nodes, err.el, h("div", { class: "mt-3" }, submit));
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.set("");
    if (!old.value) return err.set("Nhập mật khẩu hiện tại");
    const bad = pw.check();
    if (bad) return err.set(bad);
    await withBusy(submit, async () => {
      try {
        const post = (url, body) => api("POST", url, body, { quiet: true });
        const re = await scramLogin(post, ctx.me.username, old.value, "/api/me/reauth/start", "/api/me/reauth/finish");
        await api("POST", "/api/me/password", { reauth_token: re.reauth_token, credentials: await makeCredentials(pw.value()) }, { quiet: true });
        toast("Đã đổi mật khẩu. Các phiên khác đã bị đăng xuất.");
        ctx.navigate("#/databases");
      } catch (e2) {
        err.set(e2.status === 401 ? "Mật khẩu hiện tại không đúng" : e2.message);
      }
    });
  });
  ctx.root.append(pageHead("Đổi mật khẩu"), h("div", { class: "panel narrow" }, form));
}
