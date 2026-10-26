// Browser smoke test of the Fridge web UI. Run by run.sh inside the Playwright image.
// Env: BASE, SETUP_TOKEN, ADMIN_PW, MINIO_ENDPOINT. Exit 1 on the first failed step or on any console/CSP error.
import { chromium } from "playwright";

const BASE = process.env.BASE || "https://localhost:18443";
const SETUP_TOKEN = process.env.SETUP_TOKEN;
const ADMIN_PW = process.env.ADMIN_PW;
const MINIO = process.env.MINIO_ENDPOINT || "http://minio:9000";
const BOB_PW = "bob has a long password 1";

const problems = [];
let failed = 0;
let shot = 0;

async function step(name, fn) {
  try {
    await fn();
    console.log("PASS", name);
  } catch (e) {
    failed++;
    console.log("FAIL", name, "\n     ", String(e.message || e).split("\n")[0]);
    for (const [label, page] of pages) {
      await page.screenshot({ path: `fail-${++shot}-${label}.png` }).catch(() => {});
      const msgs = await page.locator(".err-text, .toast-msg, .alert").allTextContents().catch(() => []);
      if (msgs.length) console.log(`      [${label}] on screen:`, msgs.join(" | "));
    }
    await finish();
  }
}

const pages = new Map();
function watch(label, page) {
  pages.set(label, page);
  page.on("console", (m) => {
    // A 401/400 answer is logged by the browser as "Failed to load resource": expected on login and refusals.
    if (m.type() === "error" && !/Failed to load resource/.test(m.text())) problems.push(`[${label}] console: ${m.text()}`);
  });
  page.on("pageerror", (e) => problems.push(`[${label}] pageerror: ${e.message}`));
  return page.addInitScript(() =>
    document.addEventListener("securitypolicyviolation", (e) => console.error(`CSP ${e.violatedDirective} blocked ${e.blockedURI}`)),
  );
}

let browser;
async function finish() {
  await browser?.close().catch(() => {});
  if (problems.length) {
    console.log("\nconsole / CSP problems:");
    for (const p of new Set(problems)) console.log("  ", p);
  }
  console.log(failed || problems.length ? "\nSMOKE FAILED" : "\nSMOKE PASSED");
  process.exit(failed || problems.length ? 1 : 0);
}

browser = await chromium.launch();
const newPage = async (label) => {
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true, acceptDownloads: true, locale: "vi-VN" });
  const page = await ctx.newPage();
  await watch(label, page);
  page.setDefaultTimeout(20000);
  return page;
};
const admin = await newPage("admin");
const heading = (page, text) => page.locator("h1", { hasText: text }).waitFor();
const toast = (page, text, timeout = 20000) => page.locator(".toast-msg", { hasText: text }).first().waitFor({ timeout });
const modal = (page) => page.locator(".modal.show");
// Escape right after opening is swallowed by the fade-in; the close button is not.
const closeModal = async (page) => {
  await page.locator(".modal.show .btn-close").click();
  await page.locator(".modal").waitFor({ state: "detached" });
};

// ---------------------------------------------------------------- Task 19: shell, SCRAM, setup, login
await step("SCRAM test vector (RFC 7677) passes in the browser", async () => {
  await admin.goto(BASE + "/test/scram.html");
  await admin.locator("#result", { hasText: "PASS" }).waitFor();
});

await step("empty server sends the visitor to Setup", async () => {
  await admin.goto(BASE + "/");
  await admin.waitForURL(/#\/setup$/);
  await admin.getByText("Tạo tài khoản quản trị đầu tiên").waitFor();
});

await step("a bad setup token is refused", async () => {
  await admin.fill("input[name=setup-token]", "wrong-token");
  await admin.fill("input[name=username]", "root1");
  await admin.fill("input[name=new-password]", ADMIN_PW);
  await admin.fill("input[name=repeat-password]", ADMIN_PW);
  await admin.getByRole("button", { name: "Tạo tài khoản quản trị" }).click();
  await admin.locator(".err-text", { hasText: /\w/ }).first().waitFor();
});

await step("setup with the right token lands on Databases", async () => {
  await admin.fill("input[name=setup-token]", SETUP_TOKEN);
  await admin.getByRole("button", { name: "Tạo tài khoản quản trị" }).click();
  await heading(admin, "Databases");
});

await step("sidebar collapse is remembered across a reload", async () => {
  await admin.getByRole("button", { name: "Đóng/mở thanh bên" }).click();
  await admin.locator(".shell.slim").waitFor();
  await admin.reload();
  await heading(admin, "Databases");
  await admin.locator(".shell.slim").waitFor();
  await admin.getByRole("button", { name: "Đóng/mở thanh bên" }).click();
  await admin.locator(".shell:not(.slim)").waitFor();
});

await step("log out and log in again", async () => {
  await admin.getByTitle("Đăng xuất").click();
  await admin.locator("input[name=password]").waitFor();
  await admin.fill("input[name=username]", "root1");
  await admin.fill("input[name=password]", "not the password 123");
  await admin.getByRole("button", { name: "Đăng nhập" }).click();
  await admin.getByText("Sai tên đăng nhập hoặc mật khẩu").waitFor();
  await admin.fill("input[name=password]", ADMIN_PW);
  await admin.getByRole("button", { name: "Đăng nhập" }).click();
  await heading(admin, "Databases");
});

// ---------------------------------------------------------------- Task 20: databases, restore, destinations, domains
await step("register two domains", async () => {
  await admin.goto(BASE + "/#/domains");
  for (const d of ["root1.lan", "copy.root1.lan"]) {
    await admin.fill("input[name=domain]", d);
    await admin.getByRole("button", { name: "Đăng ký domain" }).click();
    await admin.locator("td", { hasText: d }).first().waitFor();
  }
});

await step("add a MinIO destination and test it", async () => {
  await admin.goto(BASE + "/#/destinations");
  await admin.getByRole("button", { name: "Thêm destination" }).first().click();
  const m = modal(admin);
  await m.locator("input[name=name]").fill("minio");
  await m.locator("input[name=endpoint]").fill(MINIO);
  await m.locator("input[name=bucket]").fill("fridge");
  await m.locator("input[name=access_key]").fill("minioadmin");
  await m.locator("input[name=secret_key]").fill("minioadmin");
  await m.locator("#dest-path").check();
  await m.getByRole("button", { name: "Lưu" }).click();
  const row = admin.locator("tr", { hasText: "minio" });
  await row.waitFor();
  await row.getByRole("button", { name: "Test" }).click();
  // a failed test shows an error toast for a few seconds: report what it said instead of timing out
  const res = await Promise.race([
    toast(admin, "kết nối được", 60000).then(() => "ok"),
    admin.locator(".toast-msg.err").first().waitFor({ timeout: 60000 }).then(async () => "error toast: " + (await admin.locator(".toast-msg.err").first().textContent())),
  ]);
  if (res !== "ok") throw new Error(res);
});

let dbUser, dbPass;
await step("create a database with generated user and password", async () => {
  await admin.goto(BASE + "/#/databases");
  await admin.getByRole("button", { name: "Tạo database" }).first().click();
  const m = modal(admin);
  await m.locator("input[name=db-name]").fill("app");
  const dice = m.locator('button[title="Ngẫu nhiên"]');
  await dice.nth(0).click();
  await dice.nth(1).click();
  dbUser = await m.locator("input[name=db-user]").inputValue();
  dbPass = await m.locator("input[name=db-password]").inputValue();
  if (!/^[a-z0-9]{8}$/.test(dbUser) || !/^[A-Za-z0-9_.~-]{24}$/.test(dbPass)) throw new Error(`odd generated values ${dbUser} ${dbPass}`);
  await m.getByRole("button", { name: "Tạo database" }).click();
  await admin.waitForURL(/#\/databases\/root1_app$/, { timeout: 180000 }); // creating a node is synchronous: container, health check, users
  await heading(admin, "root1_app");
});

await step("database becomes running and the connection string shows the generated user and password", async () => {
  await admin.locator(".status.running").first().waitFor({ timeout: 120000 });
  await admin.locator(".strip .value", { hasText: "****" }).waitFor();
  await admin.getByRole("button", { name: "Hiện mật khẩu" }).first().click();
  const want = `root1_${dbUser}:${dbPass}@`;
  await admin.locator(".strip .value", { hasText: want }).first().waitFor();
  // the CA that vouches for this database (its owner's own) is one click away
  const caHref = await admin.locator("a[download$=\"-ca.crt\"]").first().getAttribute("href");
  if (!/^\/api\/databases\/root1_[^/]+\/ca$/.test(caHref)) throw new Error("CA link: " + caHref);
});

await step("Backup now produces an ok backup row and the list shows it", async () => {
  await admin.getByRole("button", { name: "Backup now" }).click();
  const m = modal(admin);
  await m.locator("select[name=destination]").selectOption({ label: "minio" });
  await m.getByRole("button", { name: "Bắt đầu backup" }).click();
  await admin.locator("tbody .status.ok").first().waitFor({ timeout: 180000 });
  await admin.goto(BASE + "/#/databases");
  await admin.locator("tr", { hasText: "root1_app" }).locator(".status.ok").waitFor();
});

await step("Restore…: deep check passes, then restore into a new node", async () => {
  await admin.goto(BASE + "/#/databases/root1_app?tab=backups");
  await admin.getByRole("button", { name: "Restore…" }).first().click();
  await admin.waitForURL(/#\/restore\?/);
  await admin.getByText("Restore vào node mới").waitFor();
  await admin.getByRole("button", { name: "Kiểm tra kỹ" }).click();
  await admin.getByText("File đọc được trọn vẹn").waitFor({ timeout: 240000 });
  await admin.locator("input[name=db-name]").fill("copy");
  await admin.locator("select[name=domain]").selectOption("copy.root1.lan");
  await admin.locator("form").getByRole("button", { name: "Restore", exact: true }).click();
  await admin.getByText("Restore xong").waitFor({ timeout: 300000 });
});

let inviteLink;
await step("admin invites bob", async () => {
  await admin.goto(BASE + "/#/users");
  await admin.getByRole("button", { name: "Mời người dùng" }).click();
  const m = modal(admin);
  await m.locator("input[name=invite-username]").fill("bob");
  await m.getByRole("button", { name: "Tạo lời mời" }).click();
  inviteLink = await admin.locator(".modal.show .strip .value").textContent();
  if (!inviteLink.includes("/#/invite/")) throw new Error("no invite link: " + inviteLink);
  await closeModal(admin);
});

const bob = await newPage("bob");
await step("bob accepts the invite and sees nothing of the admin's", async () => {
  await bob.goto(inviteLink.replace(/^https:\/\/[^/]+/, BASE));
  await bob.getByText("Bạn được mời vào Fridge").waitFor();
  await bob.fill("input[name=new-password]", BOB_PW);
  await bob.fill("input[name=repeat-password]", BOB_PW);
  await bob.getByRole("button", { name: "Tạo tài khoản" }).click();
  await heading(bob, "Databases");
  await bob.getByText("Chưa có database nào").waitFor();
  await bob.goto(BASE + "/#/destinations");
  await bob.getByText("Chưa có destination nào").waitFor();
  await bob.goto(BASE + "/#/databases/root1_app");
  await bob.getByText("HTTP 404").or(bob.getByText(/not found/i)).first().waitFor();
});

await step("admin shares the MinIO destination; bob can use it in a schedule but not edit it", async () => {
  await admin.goto(BASE + "/#/destinations");
  await admin.getByLabel("Dùng chung minio").check();
  await toast(admin, "Đã bật dùng chung");

  await bob.goto(BASE + "/#/domains");
  await bob.fill("input[name=domain]", "bob.lan");
  await bob.getByRole("button", { name: "Đăng ký domain" }).click();
  await bob.locator("td", { hasText: "bob.lan" }).waitFor();
  await bob.goto(BASE + "/#/databases");
  await bob.getByRole("button", { name: "Tạo database" }).first().click();
  const m = modal(bob);
  await m.locator("input[name=db-name]").fill("shop");
  await m.getByRole("button", { name: "Tạo database" }).click();
  await bob.waitForURL(/#\/databases\/bob_shop$/, { timeout: 180000 });
  await bob.getByRole("tab", { name: "Lịch backup" }).click();
  await bob.getByRole("button", { name: "Thêm lịch" }).click();
  await bob.locator('.modal.show select[name=destination] option', { hasText: "minio (dùng chung)" }).waitFor({ state: "attached" });
  await closeModal(bob);
  await bob.goto(BASE + "/#/destinations");
  await bob.locator("tr", { hasText: "minio" }).waitFor();
  if ((await bob.getByRole("button", { name: "Sửa" }).count()) !== 0) throw new Error("bob can edit the shared destination");
});

await step("bob cannot point a destination at Fridge's own network", async () => {
  await bob.getByRole("button", { name: "Thêm destination" }).first().click();
  const m = modal(bob);
  await m.locator("input[name=name]").fill("evil");
  await m.locator("input[name=endpoint]").fill("http://fridge-meta:5432");
  await m.locator("input[name=bucket]").fill("bkt");
  await m.locator("input[name=access_key]").fill("ak");
  await m.locator("input[name=secret_key]").fill("sk");
  await m.getByRole("button", { name: "Lưu" }).click();
  await m.getByText("endpoint không được phép").waitFor();
  await closeModal(bob);
});

// ---------------------------------------------------------------- Task 21: users, sessions, audit, activity, images
await step("locking bob sends bob's page to the login on its next request", async () => {
  await admin.goto(BASE + "/#/users");
  await admin.locator("tr", { hasText: "bob" }).getByRole("button", { name: "Khóa" }).click();
  await modal(admin).getByRole("button", { name: "Khóa" }).click();
  await toast(admin, "Đã cập nhật bob");
  await bob.locator('a[href="#/domains"]').first().click();
  await bob.locator("input[name=password]").waitFor();
});

await step("demoting the only admin shows an error", async () => {
  await admin.goto(BASE + "/#/users");
  await admin.locator("tr", { hasText: "root1" }).getByRole("button", { name: "Hạ xuống user" }).click();
  await admin.locator(".toast-msg.err").first().waitFor();
});

await step("settings: current session is marked, another can be revoked", async () => {
  const second = await newPage("admin2");
  await second.goto(BASE + "/#/login");
  await second.fill("input[name=username]", "root1");
  await second.fill("input[name=password]", ADMIN_PW);
  await second.getByRole("button", { name: "Đăng nhập" }).click();
  await heading(second, "Databases");

  await admin.goto(BASE + "/#/settings");
  await admin.getByText("phiên hiện tại").waitFor();
  const revoke = admin.getByRole("button", { name: "Thu hồi" });
  await revoke.first().waitFor();
  await revoke.first().click();
  await modal(admin).getByRole("button", { name: "Thu hồi" }).click();
  await second.locator('a[href="#/domains"]').first().click();
  await second.locator("input[name=password]").waitFor();
});

await step("audit log lists the password reveal, filters, and exports CSV", async () => {
  await admin.goto(BASE + "/#/audit");
  await admin.locator("td", { hasText: "db.reveal_password" }).first().waitFor();
  await admin.fill("input[name=actor]", "root1");
  await admin.getByRole("button", { name: "Lọc", exact: true }).click();
  await admin.locator("td", { hasText: "db.reveal_password" }).first().waitFor();
  const bobRows = () => admin.locator("tbody td", { hasText: /^bob$/ }).count();
  for (let i = 0; i < 40 && (await bobRows()) > 0; i++) await admin.waitForTimeout(250); // the table is replaced when the filtered answer arrives
  if ((await bobRows()) !== 0) throw new Error("filter by root1 still shows bob");
  const [dl] = await Promise.all([admin.waitForEvent("download"), admin.getByRole("button", { name: "Xuất CSV" }).click()]);
  if (!dl.suggestedFilename().endsWith(".csv")) throw new Error("download " + dl.suggestedFilename());
});

await step("activity shows who is online", async () => {
  await admin.goto(BASE + "/#/activity");
  await admin.getByRole("heading", { name: /Đang online/ }).waitFor();
  await admin.locator("td", { hasText: "root1" }).first().waitFor();
});

await step("images: pull postgres:15 on every node, update check reports a state per image", async () => {
  await admin.goto(BASE + "/#/images");
  await admin.getByRole("button", { name: "Pull…" }).click();
  await modal(admin).locator('[id="pull-postgres:15"]').check();
  await modal(admin).getByRole("button", { name: "Pull", exact: true }).click();
  await admin.locator(".status.ok", { hasText: "xong" }).first().waitFor({ timeout: 600000 });
  await admin.locator("tr", { hasText: "postgres:15" }).getByText("đã có").waitFor();

  await admin.getByRole("button", { name: "Kiểm tra cập nhật" }).click();
  await admin.locator(".toast-msg", { hasText: "Đã kiểm tra" }).first().waitFor({ timeout: 180000 }); // one registry call per image
  const present = admin.locator("tr", { hasText: "đã có" });
  for (let i = 0; i < (await present.count()); i++) {
    const t = await present.nth(i).textContent();
    if (!/mới nhất|New version|không kiểm tra được/.test(t)) throw new Error("no update state in: " + t);
  }
});

await step("every page renders without console or CSP errors", async () => {
  for (const r of ["databases", "restore", "destinations", "domains", "audit", "settings", "users", "sessions", "images", "activity", "password"]) {
    await admin.goto(`${BASE}/#/${r}`);
    await admin.locator("h1").first().waitFor();
  }
});

await finish();
