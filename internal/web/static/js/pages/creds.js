// Database name, user and password fields shared by "create database" and "restore into a new node".
import { h, icon, field, input, randomSuffix, randomPassword, copyText } from "../ui.js";

export const NAME_RE = /^[a-z0-9][a-z0-9-]{0,29}$/;
export const USER_RE = /^[a-z0-9_]{1,15}$/;
export const PASS_RE = /^[\x21-\x7e]{12,128}$/;

function prefixed(prefix, el) {
  return h("div", { class: "input-group" }, h("span", { class: "input-group-text input-prefix" }, prefix), el);
}

// credentialFields(owner) -> {nodes, values(), check()}. Empty user/password mean "let the server choose".
export function credentialFields(owner) {
  const prefix = owner + "_";
  const name = input({ name: "db-name", placeholder: "vd: shop", maxlength: "30" });
  const user = input({ name: "db-user", placeholder: "để trống = giống tên database", maxlength: "15" });
  const pw = input({ type: "password", name: "db-password", placeholder: "để trống = máy chủ tự sinh", autocomplete: "new-password" });
  const dice = (label, fn) => h("button", { type: "button", class: "btn btn-outline-secondary", title: label, "aria-label": label, onclick: fn }, icon("dice-5"));

  const userRow = h("div", { class: "input-group" }, h("span", { class: "input-group-text input-prefix" }, prefix), user, dice("Ngẫu nhiên", () => (user.value = randomSuffix())));
  const eye = h("button", { type: "button", class: "btn btn-outline-secondary", title: "Hiện/ẩn", "aria-label": "Hiện hoặc ẩn mật khẩu" }, icon("eye"));
  eye.addEventListener("click", () => {
    pw.type = pw.type === "password" ? "text" : "password";
    eye.replaceChildren(icon(pw.type === "password" ? "eye" : "eye-slash"));
  });
  const pwRow = h(
    "div",
    { class: "input-group" },
    pw,
    eye,
    h("button", { type: "button", class: "btn btn-outline-secondary", title: "Sao chép", "aria-label": "Sao chép mật khẩu", onclick: () => pw.value && copyText(pw.value) }, icon("clipboard")),
    dice("Ngẫu nhiên", () => {
      pw.value = randomPassword();
      pw.type = "text";
      eye.replaceChildren(icon("eye-slash"));
    }),
  );

  return {
    nodes: [
      field("Tên database", prefixed(prefix, name), "Chữ thường, số và dấu gạch ngang, tối đa 30 ký tự."),
      field("User", userRow, "Chữ thường, số và dấu gạch dưới, tối đa 15 ký tự."),
      field("Mật khẩu", pwRow, "12–128 ký tự, không có khoảng trắng. Ghi lại nếu bạn tự đặt: sau này chỉ xem được qua nút Hiện (có ghi audit)."),
    ],
    name,
    values: () => ({ name: name.value.trim(), db_user: user.value.trim(), db_password: pw.value }),
    // check returns an error text, "" when fine.
    check(needName = true) {
      if (needName && !NAME_RE.test(name.value.trim())) return "Tên database: chữ thường, số, dấu gạch ngang; bắt đầu bằng chữ hoặc số; tối đa 30 ký tự";
      if (user.value && !USER_RE.test(user.value.trim())) return "User: chữ thường, số, dấu gạch dưới; tối đa 15 ký tự";
      if (pw.value && !PASS_RE.test(pw.value)) return "Mật khẩu cần 12–128 ký tự in được, không có khoảng trắng";
      return "";
    },
  };
}
