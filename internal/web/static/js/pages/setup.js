import { api } from "../api.js";
import { makeCredentials, scramLogin } from "../scram.js";
import { h, gate, field, input, passwordPair, errorBox, withBusy, USERNAME_RE, USERNAME_HINT, icon } from "../ui.js";

const post = (url, body) => api("POST", url, body, { quiet: true });

export default async function setup(ctx) {
  const st = await api("GET", "/api/setup/status", undefined, { quiet: true });
  if (!st.needed) return ctx.navigate("#/login");

  const token = input({ type: "password", name: "setup-token", autocomplete: "off" });
  const user = input({ name: "username", autocomplete: "username" });
  const pw = passwordPair();
  const err = errorBox();
  const submit = h("button", { type: "submit", class: "btn btn-primary w-100", disabled: !st.token_configured }, "Tạo tài khoản quản trị");

  const form = h(
    "form",
    { novalidate: true },
    !st.token_configured
      ? h("div", { class: "alert alert-warning", role: "alert" }, icon("exclamation-triangle"), " Chưa cấu hình setup token. Đặt FRIDGE_SETUP_TOKEN cho máy chủ rồi khởi động lại; sau đó tải lại trang này.")
      : null,
    field("Setup token", token, "Giá trị FRIDGE_SETUP_TOKEN của máy chủ."),
    field("Tên đăng nhập", user, USERNAME_HINT),
    pw.nodes,
    err.el,
    h("div", { class: "mt-3" }, submit),
  );
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.set("");
    if (!USERNAME_RE.test(user.value)) return err.set("Tên đăng nhập: " + USERNAME_HINT);
    if (!token.value) return err.set("Cần setup token");
    const bad = pw.check();
    if (bad) return err.set(bad);
    await withBusy(submit, async () => {
      try {
        const credentials = await makeCredentials(pw.value());
        await post("/api/setup", { setup_token: token.value, username: user.value, credentials });
        await scramLogin(post, user.value, pw.value(), "/api/auth/scram/start", "/api/auth/scram/finish");
        await ctx.signedIn();
      } catch (e2) {
        err.set(e2.message);
      }
    });
  });
  gate(ctx.root, "Tạo tài khoản quản trị đầu tiên.", form);
  user.focus();
}
