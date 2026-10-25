import { api } from "../api.js";
import { makeCredentials, scramLogin } from "../scram.js";
import { h, gate, passwordPair, errorBox, withBusy } from "../ui.js";

const post = (url, body) => api("POST", url, body, { quiet: true });

// The invite token lives in the URL hash only (never sent to the server as part of a URL):
// it travels in the body of the two requests below.
export default async function invite(ctx) {
  const token = ctx.params.token;
  let info;
  try {
    info = await post("/api/invites/lookup", { token });
  } catch {
    return gate(ctx.root, "Lời mời này không dùng được.", h("p", {}, "Lời mời hết hạn hoặc đã được dùng. Nhờ quản trị viên gửi lại một lời mời mới."), h("a", { href: "#/login" }, "Về trang đăng nhập"));
  }
  const reset = info.purpose === "reset_password";
  const pw = passwordPair(reset ? "Mật khẩu mới" : "Mật khẩu");
  const err = errorBox();
  const submit = h("button", { type: "submit", class: "btn btn-primary w-100" }, reset ? "Đặt mật khẩu mới" : "Tạo tài khoản");
  const form = h("form", { novalidate: true }, h("p", {}, "Tài khoản: ", h("strong", { class: "mono" }, info.username)), pw.nodes, err.el, h("div", { class: "mt-3" }, submit));
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.set("");
    const bad = pw.check();
    if (bad) return err.set(bad);
    await withBusy(submit, async () => {
      try {
        const credentials = await makeCredentials(pw.value());
        await post("/api/invites/accept", { token, credentials });
        await scramLogin(post, info.username, pw.value(), "/api/auth/scram/start", "/api/auth/scram/finish");
        await ctx.signedIn();
      } catch (e2) {
        err.set(e2.message);
      }
    });
  });
  gate(ctx.root, reset ? "Đặt lại mật khẩu." : "Bạn được mời vào Fridge.", form);
}
