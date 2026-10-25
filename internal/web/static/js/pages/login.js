import { api } from "../api.js";
import { scramLogin } from "../scram.js";
import { h, gate, field, input, errorBox, withBusy } from "../ui.js";

const post = (url, body) => api("POST", url, body, { quiet: true });

export default async function login(ctx) {
  const user = input({ name: "username", autocomplete: "username" });
  const pw = input({ type: "password", name: "password", autocomplete: "current-password" });
  const err = errorBox();
  const submit = h("button", { type: "submit", class: "btn btn-primary w-100" }, "Đăng nhập");
  const form = h("form", { novalidate: true }, field("Tên đăng nhập", user), field("Mật khẩu", pw), err.el, h("div", { class: "mt-3" }, submit));
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.set("");
    if (!user.value || !pw.value) return err.set("Nhập tên đăng nhập và mật khẩu");
    await withBusy(submit, async () => {
      try {
        await scramLogin(post, user.value.trim(), pw.value, "/api/auth/scram/start", "/api/auth/scram/finish");
        await ctx.signedIn();
      } catch (e2) {
        err.set(e2.status === 429 ? "Thử sai quá nhiều lần. Đợi vài phút rồi thử lại." : e2.status === 401 ? "Sai tên đăng nhập hoặc mật khẩu" : e2.message);
        pw.value = "";
      }
    });
  });
  gate(ctx.root, "Đăng nhập để quản lý database.", form);
  user.focus();
}
