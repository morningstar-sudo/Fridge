# Fridge — Web admin, tài khoản & phân quyền theo tài nguyên (giai đoạn 2)

Bổ sung cho `2026-10-20-fridge-design.md`. Giai đoạn này được người dùng **đưa lên trước** health/monitor/limits. Nền tảng: giai đoạn 1 đã xong (Postgres, Traefik SNI, backup/restore S3, REST API, CLI) trên nhánh `feat/phase1-postgres`.

## Vì sao

Hiện chỉ có CLI + một token tĩnh `FRIDGE_TOKEN` dùng chung: ai có token làm được mọi thứ, hệ thống không biết ai là ai. Team cần một giao diện web để làm việc hằng ngày, có tài khoản riêng, và mỗi người chỉ quản lý tài nguyên của mình.

## Mục tiêu

- **Web admin** dùng **Vanilla JS + Bootstrap 5** (người dùng chốt), nhúng trong binary `fridge`, phủ các luồng của giai đoạn 1.
- **Tài khoản**: lần đầu bắt buộc tạo admin bằng setup token trong env; đăng nhập bằng **SCRAM-SHA-256** (mật khẩu không bao giờ rời trình duyệt/CLI); **session stateful** lưu server-side.
- **Vai trò**: `admin` (quản lý hệ thống và **toàn bộ** tài nguyên) và `user`.
- **Sở hữu theo tài nguyên**: DB và S3 destination có owner; owner DB có thể thêm người quản lý; destination có thể được admin đánh dấu **dùng chung**.
- **HTTPS mặc định** cho web admin + API bằng cert tự ký tự sinh khi dựng server.
- CLI đăng nhập bằng cùng cơ chế (session của chính user), **bỏ `FRIDGE_TOKEN` tĩnh**.

## Non-goals (giai đoạn này)

- RBAC chi tiết hơn admin/user; nhóm/team.
- **API token** cho script/CI — sẽ là một hệ thống **riêng**, không dùng chung với session ID (người dùng chốt). Thiết kế middleware xác thực sao cho thêm loại thứ hai không phải viết lại.
- Upload cert SSL tùy chỉnh (sau).
- Trang health/monitor/limits (giai đoạn 3), audit log, xóa DB, restore ghi đè (giai đoạn sau).
- Log chạy trực tiếp (SSE) — dùng polling.

## Anti-goals

1. Mật khẩu (hoặc thứ gì đủ để đăng nhập thay mật khẩu) đi qua mạng hoặc nằm trong DB ở dạng dùng lại được.
2. User thấy hoặc đụng được DB/destination/backup không thuộc quyền mình — kể cả gián tiếp (restore bản backup của người khác, đoán id).
3. Tự khóa mình không có đường thoát (quên mật khẩu admin duy nhất, allowlist chặn chính mình).
4. Dùng web khó hơn CLI.

## Ràng buộc

- Một controller duy nhất (không cần store phân tán; rate-limit và trạng thái SCRAM dở dang để trong RAM là được).
- Không thêm dependency Go mới (SCRAM, PBKDF2, HMAC dùng stdlib: `crypto/pbkdf2`, `crypto/hmac`, `crypto/sha256`). Bootstrap 5 + Bootstrap Icons **vendored** vào `internal/web/static/vendor/` (mạng nội bộ có thể không có internet) — không CDN, không build step, không npm.
- Data plane vẫn độc lập với Fridge (anti-goal của spec gốc).

## Thiết kế

### 1. Mô hình dữ liệu (migration mới, không sửa migration cũ)

- `users(id, username UNIQUE, role[admin|user], scram_salt BYTEA, scram_iterations INT, stored_key BYTEA, server_key BYTEA, disabled BOOL, created_at, created_by NULL)`
- `invites(id, token_hash UNIQUE, username, role, purpose[new_user|reset_password], user_id NULL, created_by, expires_at, used_at NULL)`
- `sessions(id_hash UNIQUE, user_id, created_at, last_seen_at, expires_at, ip, user_agent)` — `id_hash` = SHA-256 của session ID; DB lộ cũng không dùng được.
- `databases` thêm `owner_id BIGINT NULL REFERENCES users(id)`.
- `database_members(database_id, user_id, added_by, added_at, PRIMARY KEY(database_id, user_id))`.
- `destinations` thêm `owner_id BIGINT NULL REFERENCES users(id)`, `shared BOOL NOT NULL DEFAULT false`.
- **Dữ liệu cũ**: DB/destination có `owner_id NULL` được gán cho admin đầu tiên ngay khi bước setup tạo admin xong.

### 1b. Ràng buộc toàn vẹn (người dùng yêu cầu "siết chặt quan hệ")

- **Khóa ngoại đầy đủ**, mọi cột id là `BIGINT`:
  - `backups.destination_id`, `restores.destination_id`, `dump_checks.destination_id` → `destinations`.
  - `backups.schedule_id` → `backup_schedules` `ON DELETE SET NULL`: xóa lịch vẫn giữ lịch sử backup; backup đó không còn chịu retention.
  - `invites`, `sessions`, `database_members` → `users`. Ngoài ra `database_members` → `databases` và `*_by_id` → `users`.
- **Xóa destination = xóa mềm** (người dùng chốt): thêm cột `deleted_at`.
  - Khi xóa: đặt `deleted_at`, xóa `secret_key_enc` và `access_key`, ẩn khỏi mọi danh sách và dropdown. Lịch sử backup/restore vẫn trỏ đúng tới bản ghi.
  - Vẫn chặn xóa khi còn lịch backup trỏ tới destination đó.
  - Destination đã xóa không còn tải, restore hay test được: báo "destination đã bị xóa".
  - **Tên destination unique theo từng owner**, chỉ tính các destination chưa xóa.
- **`owner_id NOT NULL`** trên `databases` và `destinations`, áp trong chính transaction tạo admin đầu tiên, sau khi gán owner cho dữ liệu cũ.
- **CHECK** cho mọi cột liệt kê:
  - trạng thái DB/backup/restore/dump_check, engine, `tls_mode`, `trigger`, `mode`, `role`, `purpose`;
  - `size_bytes ≥ 0`, `keep_latest ≥ 1`, cron không rỗng;
  - độ dài salt 16 byte và khóa SCRAM 32 byte, `iterations` ∈ [600000, 10000000];
  - `expires_at > created_at`.
- **Nhất quán theo trạng thái:**
  - Job `running` ⇔ `finished_at IS NULL`.
  - Backup `ok` ⇒ `s3_key` khác rỗng.
  - Lời mời `reset_password` ⇔ có `user_id`.
- **Trigger:** owner không được nằm trong danh sách người quản lý chính DB của mình.
- **Nâng cấp dữ liệu giai đoạn 1:**
  - Backup/restore trỏ tới destination đã bị xóa cứng trước đây được gắn vào một bản ghi destination "tombstone" (đã xóa mềm, giữ nguyên id cũ).
  - `schedule_id` mồ côi được đặt NULL.
  - Sau đó mới thêm khóa ngoại.

### 2. Khởi tạo lần đầu

- Env mới **`FRIDGE_SETUP_TOKEN`** (người dùng chốt: "1 token trong env").
- Khi bảng `users` **rỗng**: mọi trang web chuyển tới màn **Tạo admin** (nhập setup token, username, mật khẩu ×2). Sai token → từ chối (so sánh constant-time).
- Khi **đã có user**: bỏ qua hoàn toàn — endpoint setup trả 409, setup token vô hiệu, server khởi động được dù không có biến này.
- Chưa có user mà thiếu `FRIDGE_SETUP_TOKEN` → server vẫn chạy nhưng log cảnh báo rõ và màn setup báo "chưa cấu hình setup token".
- Tài khoản tạo ở bước này là `admin`.

### 3. SCRAM-SHA-256 (RFC 5802 / RFC 7677)

- Dùng **đúng định dạng message chuẩn** (`n,,n=<user>,r=<cnonce>` …), bọc trong JSON. Nhờ vậy test được bằng test vector RFC 7677 và CLI/JS dùng cùng thuật toán.
- PBKDF2-HMAC-SHA-256, **600 000 vòng** (OWASP), salt 16 byte ngẫu nhiên. *(Lựa chọn của Claude.)*
- **Đăng ký** (setup, chấp nhận lời mời, đổi mật khẩu): client tự tính `SaltedPassword` → gửi `salt, iterations, stored_key, server_key`. Server không bao giờ thấy mật khẩu. **Client sinh salt** (16 byte ngẫu nhiên từ `crypto.getRandomValues`). Mật khẩu tối thiểu 12 ký tự — kiểm ở client (server không thấy mật khẩu; ghi rõ đây là kiểm soát phía client).
- **Đăng nhập**: 2 lượt:
  - `POST /api/auth/scram/start {client_first}` → `{server_first, scram_id}` (`scram_id` = khóa trạng thái trong RAM, sống 60s).
  - `POST /api/auth/scram/finish {scram_id, client_final}` → kiểm `ClientProof`; đúng → tạo session, trả `{server_final, user}`. Client **phải** kiểm `ServerSignature` (chống server giả).
- **Không lộ username có tồn tại hay không**: username không có/bị khóa vẫn trả `server_first` với salt giả xác định (`HMAC(FRIDGE_SECRET_KEY, username)` cắt 16 byte) và lỗi chung "sai tên đăng nhập hoặc mật khẩu".
- **Chống dò**: sai 5 lần trong 1 phút theo (username, IP) → khóa 5 phút (RAM). *(Lựa chọn của Claude.)*
- Đổi mật khẩu: phải chứng minh mật khẩu cũ bằng một vòng SCRAM mới (re-auth) trong cùng request flow, rồi gửi bộ khóa mới; mọi session khác của user bị xóa.

### 4. Session stateful (người dùng chốt)

- Session ID: 32 byte ngẫu nhiên (base64url). Server lưu hash trong `sessions`.
- **Web**: cookie `fridge_session`, `HttpOnly; Secure; SameSite=Strict; Path=/`.
- **CLI**: `Authorization: Bearer <session id>` — cùng bảng, cùng quyền. `fridge login` chạy SCRAM, lưu session ID vào `~/.config/fridge/session` (0600); env `FRIDGE_TOKEN` (nếu đặt) ghi đè file. `fridge logout` xóa session phía server + file. `fridge whoami`.
- Hết hạn: không hoạt động **12 giờ** hoặc tuyệt đối **7 ngày**; `last_seen_at` cập nhật tối đa 1 lần/phút. Job dọn session hết hạn mỗi giờ. *(Lựa chọn của Claude.)*
- **CSRF**: request dùng cookie và không phải GET/HEAD phải có `Origin` (hoặc `Sec-Fetch-Site: same-origin`) khớp host; request dùng Bearer không cần.
- Middleware xác thực trả về một `Principal{UserID, Role}`; nguồn hiện tại: cookie hoặc Bearer session. API token sau này là nguồn thứ ba.

### 5. Vai trò & quyền

| Hành động | admin | user |
|---|---|---|
| DB mình là owner hoặc người quản lý: xem, connection string, backup, lịch, restore | ✓ | ✓ |
| DB của người khác | ✓ tất cả | ✗ — **404** (không lộ tồn tại) |
| Tạo DB (người tạo = owner) | ✓ | ✓ |
| Thêm/gỡ người quản lý DB | ✓ | chỉ **owner** |
| Destination của mình: thêm/sửa/xóa/test/duyệt bucket | ✓ | ✓ |
| Destination của người khác | ✓ tất cả | ✗ (404) — trừ khi `shared` |
| Đánh dấu/bỏ đánh dấu destination **dùng chung** | ✓ | ✗ |
| Destination `shared`: chọn khi đặt lịch backup | ✓ | ✓ (không sửa/xóa/xem secret) |
| Allowlist, cert, cài đặt hệ thống | ✓ | ✗ (403) |
| Tạo lời mời user, khóa/mở, nâng/hạ admin, reset mật khẩu | ✓ | ✗ |
| Xem/thu hồi session | mọi session | session của mình |
| Đổi mật khẩu của mình | ✓ | ✓ |

- Người quản lý DB (thêm bởi owner) làm được mọi việc trên DB trừ thêm/gỡ người quản lý.
- Destination do admin tạo **mặc định riêng**; chỉ thành dùng chung khi admin **đánh dấu** (người dùng chốt).
- Đặt lịch backup: chỉ chọn destination của chính mình hoặc `shared`. Lịch có sẵn vẫn chạy với destination cũ kể cả khi người đặt mất quyền.
- **Duyệt file để restore**:
  - Trên destination **của mình** (hoặc admin): duyệt toàn bucket, kể cả file upload tay.
  - Trên destination **shared**: không liệt kê bucket; chỉ thấy các bản backup (từ bảng `backups`) thuộc DB mình có quyền.
  - Nút "Restore…" trên một dòng backup của DB mình có quyền luôn dùng được, bất kể destination thuộc ai.
- Restore vào node mới: người bấm = owner node mới.
- Không cho hạ quyền/khóa **admin hoạt động cuối cùng**.
- Job hệ thống (scheduler, recovery khi khởi động) chạy không gắn user.

### 6. Quản lý user

- Admin tạo **lời mời** (username, role) → link dùng một lần `https://<host>:8443/#/invite/<token>`, hết hạn **48 giờ**; người được mời tự đặt mật khẩu bằng SCRAM. Người tạo không bao giờ biết mật khẩu. *(Lựa chọn của Claude.)*
- Reset mật khẩu = lời mời `purpose=reset_password` cho user có sẵn; dùng xong xóa mọi session của user đó.
- Khóa user → xóa mọi session, chặn đăng nhập. DB/destination của user bị khóa vẫn nằm đó; admin vẫn quản lý được.

### 7. HTTPS (người dùng chốt: mặc định tự ký, tự sinh khi dựng server)

- Khi khởi động, nếu chưa có, Fridge ký cert cho web admin bằng **CA nội bộ** đã có, SAN = `localhost`, `127.0.0.1` + các tên trong env tùy chọn **`FRIDGE_PUBLIC_HOSTS`** (phân tách bằng dấu phẩy, tên hoặc IP). Cert này được đặt làm **cert mặc định của Traefik** (`tls.stores.default.defaultCertificate`), nên truy cập bằng bất kỳ tên/IP nào cũng có HTTPS (trình duyệt cảnh báo nếu tên không khớp/chưa cài CA).
- Cert sinh lại khi `FRIDGE_PUBLIC_HOSTS` đổi hoặc còn < 30 ngày.
- Traefik: entrypoint `websecure :8443` (TLS) phục vụ web + API; entrypoint `web :8080` chỉ **redirect** sang HTTPS. *(Cổng do Claude chọn.)* Compose publish `8443` và `8080`.
- Trang Settings có nút tải CA cert để team cài một lần.
- Cert SSL tùy chỉnh: để sau (chừa chỗ: file cert/key do người dùng đặt sẽ thắng cert tự sinh).

### 8. Web admin (Vanilla JS + Bootstrap 5)

- File tĩnh trong `internal/web/static/` nhúng bằng `embed`, phục vụ ở `/` cùng server với API. Một trang HTML + JS module, **router theo hash** (`#/databases`, `#/databases/abc`, …). Không build step.
- Header bảo mật: `Content-Security-Policy: default-src 'self'` (không inline script/style), `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`.
- **Bố cục** (người dùng duyệt mockup A): **sidebar trái đóng/mở được** (thu gọn còn icon, trạng thái nhớ trong `localStorage`), mục: Databases · Restore · Destinations · Settings (admin thấy thêm Users, Sessions toàn hệ thống). Góc dưới sidebar: tên user · Đổi mật khẩu · Đăng xuất.
- **Màn hình**:
  - **Tạo admin** (lần đầu) · **Đăng nhập** · **Chấp nhận lời mời** · **Đổi mật khẩu**.
  - **Databases**: bảng (tên, engine/version, domain, trạng thái, backup gần nhất, owner — admin thấy cột owner), nút "+ Tạo DB" (form: engine, version dropdown từ `/api/engines`, tên, domain).
  - **Chi tiết DB** (mockup đã duyệt): header (tên, trạng thái, version, domain, **Backup now**); tab **Kết nối** (connection string mật khẩu ẩn + Hiện + Copy, host/user/db, link tải CA); tab **Backups** (bảng: thời gian, nguồn lịch/tay, destination, dung lượng, trạng thái, Tải · Restore… · Log); tab **Lịch backup** (CRUD, chọn destination của mình/shared); tab **Người quản lý** (owner/admin thêm/gỡ theo username).
  - **Restore**: chọn destination → duyệt (theo quy tắc mục 5) → chọn file → tên/domain/version node mới → Restore → trang trạng thái polling.
  - **Destinations**: bảng của mình (admin: tất cả + cột owner + công tắc "dùng chung"), thêm/sửa/xóa/Test.
  - **Settings**: allowlist (admin), tải CA cert, phiên đăng nhập của tôi (thu hồi). Admin: **Users** (mời, khóa/mở, nâng/hạ, reset mật khẩu), **Sessions** toàn hệ thống.
- Thao tác chạy nền (backup, restore) cập nhật bằng **polling** 2–3 giây.

### 9. API

- Mọi route `/api/*` hiện có giữ nguyên đường dẫn nhưng **áp quyền** theo mục 5 (lọc danh sách, 404 cho tài nguyên không thuộc quyền, 403 cho thao tác cấm trên tài nguyên thấy được).
- Thêm (tên endpoint do builder quyết, gợi ý): `GET /api/setup/status`, `POST /api/setup`, `POST /api/auth/scram/start|finish`, `POST /api/auth/logout`, `GET /api/me`, `POST /api/me/password`, sessions (list/revoke), users + invites (admin), `GET|POST|DELETE /api/databases/{db}/members`, `PATCH /api/destinations/{id}` (cờ `shared`, admin).
- Response JSON có thêm `owner` (username) cho DB/destination.

### 10. Đường thoát khi tự khóa (anti-goal 3)

Lệnh chạy **bên trong container** (`docker exec <fridge> /fridge recover …`), đọc env của server và thao tác thẳng vào DB + file Traefik, không qua API/đăng nhập:
- `fridge recover allowlist <cidr>...` — đặt lại allowlist.
- `fridge recover reset-password <username>` — in link lời mời reset mật khẩu.
- `fridge recover promote <username>` — nâng một user thành admin.

### 11. Cấu hình

- **Bỏ** `FRIDGE_TOKEN` khỏi server, compose, `.env.example`, README.
- **Thêm** `FRIDGE_SETUP_TOKEN` (cần khi chưa có user), `FRIDGE_PUBLIC_HOSTS` (tùy chọn).
- CLI: `FRIDGE_URL` mặc định `https://localhost:8443`; CLI tin CA nội bộ qua `--ca-file`/`FRIDGE_CA_FILE` hoặc `fridge login --insecure` lần đầu để tải CA (builder quyết chi tiết, nhưng mặc định **không** tắt kiểm tra TLS).

### 12. Quyết định bổ sung (lựa chọn của Claude, sau builder check)

- **IP thật của client**: Fridge chỉ tin `X-Forwarded-For` khi request đến từ Traefik (env `FRIDGE_TRUSTED_PROXY`, mặc định hostname `traefik`, resolve lúc khởi động); container fridge không publish port. IP này dùng cho khóa đăng nhập, `sessions.ip` và log. CSRF so `Origin` với `Host` mà Traefik chuyển nguyên.
- **CA cert công khai**: `GET /api/ca` **không cần đăng nhập** (chỉ là cert công khai). `fridge login` lần đầu chưa có CA → tải CA, in fingerprint SHA-256, hỏi xác nhận (TOFU), lưu vào `~/.config/fridge/ca.pem` và dùng từ đó; `--ca-file`/`FRIDGE_CA_FILE` để chỉ định sẵn. Không có chế độ tắt kiểm tra TLS.
- **Traefik**: cert mặc định không ảnh hưởng route DB — các router `:5432` đã chọn cert theo SNI (`tls.certificates` từng domain); SNI lạ vốn không có router. Đổi entrypoint `api :8080` → `websecure :8443` + `web :8080` (redirect) được chấp nhận; `ping :8082` giữ nguyên.
- **Backup khi destination không còn trong tầm nhìn**: ai có quyền với DB thì luôn thấy, tải (presign phía server), restore các bản backup của DB đó, bất kể destination thuộc ai hay đã bỏ dùng chung. Không cho xóa destination khi còn lịch backup (của bất kỳ ai) trỏ tới (giữ quy tắc ErrInUse hiện có). Bỏ cờ dùng chung không phá lịch có sẵn của người khác.
- **Người quản lý DB**: xem connection string (kể cả mật khẩu), Backup now, đặt lịch (destination của mình hoặc shared), restore vào node mới (họ là owner node mới) — đúng như bảng mục 5.
- **SCRAM**: server từ chối đăng ký với `iterations < 600000` hoặc `> 10000000`, salt khác 16 byte. User giả trả `iterations = 600000` cố định; bước finish trả cùng lỗi chung và có thời gian xử lý tương đương. **Re-auth** (đổi mật khẩu): một vòng SCRAM start/finish với `purpose=reauth` → trả `reauth_token` dùng một lần, sống 2 phút, gắn với session hiện tại; `POST /api/me/password` cần token này.
- **Nâng cấp từ giai đoạn 1** (chưa có user): bắt buộc làm setup. Trong lúc chưa setup, `/api/*` (trừ setup/status/ca/healthz/readyz) trả **401 `{"error":"setup required"}`**; CLI in hướng dẫn mở web để setup.
- **Host trong link mời**: lấy từ `Host` của request tạo lời mời; lệnh `recover reset-password` (không có request) dùng tên đầu tiên trong `FRIDGE_PUBLIC_HOSTS`, không có thì `localhost`.
- **Kiểm thử JS**: trang `/test/scram.html` (+ file JS riêng, tuân CSP) chạy test vector RFC 7677 trong trình duyệt, hiện PASS/FAIL. Vô hại nên nhúng luôn; không cần Node/npm.

### 13. Validate file dump trước khi restore (người dùng chốt: làm cả 2 mức, mức 2 là một nút "Kiểm tra kỹ")

Áp dụng cho mọi đường restore (web, CLI, API).

**Mức 1 — kiểm tra phần đầu file (luôn chạy, đồng bộ, trước khi tạo node hay bản ghi restore):**
- Đọc tối đa 64 KB đầu object bằng range GET (thêm `ReadHead(ctx, key, n)` vào ObjectStore).
- Từ chối: object rỗng; magic bytes không khớp đuôi/engine — `.dump` phải bắt đầu `PGDMP`; `.gz` phải có header gzip và giải nén được đoạn đầu; `.sql`/`.sql.gz` (sau giải nén) phải là UTF-8 hợp lệ, không chứa byte NUL, và có dấu hiệu SQL dump (dòng chú thích `-- PostgreSQL database dump`, hoặc câu lệnh `SET`/`CREATE`/`COPY`/`INSERT` trong phần đầu).
- Với `.dump`: parse header archive custom → lấy **version archive, version Postgres nguồn, version pg_dump**. Dùng version Postgres nguồn này cho kiểm tra tương thích (ưu tiên hơn metadata S3 và đường dẫn). Từ chối nếu header cho thấy định dạng không phải custom (tar/directory) hoặc version archive mới hơn pg_restore của node đích đọc được.
- Lỗi → 400 với lý do cụ thể (vd "file không phải pg_dump custom format", "dump từ Postgres 17 không restore được vào 16"); không tạo gì.

**Mức 2 — "Kiểm tra kỹ" (tùy chọn, một nút riêng cạnh Restore):**
- Chạy nền như một job: bảng `dump_checks(id, destination_id, s3_key, status[running|ok|failed], result JSONB, log, started_by, started_at, finished_at)`; polling như backup/restore.
- Tải **toàn bộ** object, stream (không ghi đĩa) vào một container tạm `postgres:<version cao nhất trong catalog>` (không network, tự xóa sau khi xong, timeout 6h):
  - `.dump`: `pg_restore -f /dev/null` đọc từ stdin — đọc hết mọi khối dữ liệu, phát hiện file cụt/khối hỏng.
  - `.sql.gz`: giải nén hết (kiểm CRC + độ dài cuối gzip) và kiểm UTF-8 toàn bộ; `.sql`: kiểm UTF-8 toàn bộ. (Không thực thi SQL — chỉ kiểm tính toàn vẹn.)
- Kết quả hiển thị: OK/lỗi, dung lượng đã đọc, version nguồn, số mục trong TOC (với `.dump`), log lỗi.
- Không bắt buộc trước khi Restore; nút Restore vẫn bấm được (người dùng tự quyết).
- Quyền: giống quy tắc duyệt file restore (mục 5).
- CLI: `fridge check --from <dest>:<key> [--wait]`; API: `POST /api/dump-checks`, `GET /api/dump-checks/{id}` (tên do builder quyết).

### 13b. User/mật khẩu của DB do người dùng đặt (người dùng chốt)

- Form **Tạo DB** và form **restore vào node mới** có ô **User** và **Mật khẩu**, mỗi ô có nút **Ngẫu nhiên** sinh chuỗi trên trình duyệt (`crypto.getRandomValues`): hậu tố user 8 ký tự `[a-z0-9]`, mật khẩu 24 ký tự `[A-Za-z0-9-_.~]`; ô mật khẩu có Hiện/Ẩn + Copy.
- **Tên user bắt buộc có tiền tố `<username owner>_`** (người dùng chốt — tránh lộ tài khoản tenant khác qua kiểm tra trùng tên): người dùng chỉ nhập hậu tố `^[a-z0-9_]{1,15}$`, UI hiện tiền tố cố định; trùng tên chỉ kiểm trong phạm vi owner; áp cho mọi engine. Username tài khoản Fridge vì vậy siết thành `^[a-z][a-z0-9_]{2,15}$` (không `.`/`-`, ≤ 16 ký tự → tổng ≤ 32 ký tự, vừa giới hạn MySQL).
- Để trống → hậu tố = hậu tố tên DB (`-` → `_`), mật khẩu `crypto/rand.Text()`. *(Lựa chọn của Claude, giữ tương thích CLI/API.)*
- Ràng buộc *(lựa chọn của Claude)*: mật khẩu 12–128 ký tự ASCII in được, không khoảng trắng. Sai → 400.
- Mật khẩu DB đi lên server qua HTTPS (bắt buộc, vì Fridge phải tạo user trong Postgres) và lưu mã hóa AES-GCM như hiện tại. Connection string dựng bằng `url.UserPassword` để ký tự đặc biệt đúng.
- Tạo/restore DB với user/mật khẩu tự đặt được ghi audit (không ghi mật khẩu).
- CLI: `fridge create … --db-user U --db-password-stdin` hoặc `--random-credentials` (CLI tự sinh như trên và in ra một lần).

### 13b-2. Hai user trên mỗi node DB (người dùng chốt)

- **Superuser riêng của từng DB** (người dùng chốt, thay cho phương án dùng chung theo owner — để áp được cho mọi engine, kể cả MySQL/MariaDB route theo username qua ProxySQL): mỗi lần tạo/restore node, Fridge sinh superuser `su_<node_key>` (mục 13b-3) + mật khẩu ngẫu nhiên (`crypto/rand.Text()`), lưu mã hóa trong `databases.admin_user` / `databases.admin_password_enc`. Tên duy nhất theo DB đang hoạt động.
  - Container khởi tạo với `POSTGRES_USER = su_<node_key>`, `POSTGRES_PASSWORD = mật khẩu superuser`, `POSTGRES_DB = <tên DB>`. Healthcheck, backup (`pg_dump`), restore, kiểm tra kỹ chạy bằng superuser này (qua socket nội bộ, không cần mật khẩu).
  - **Owner** và **admin** xem và **rotate** superuser của DB (rotate: `ALTER ROLE` trên node, thành công mới lưu mật khẩu mới). **Người quản lý không thấy** *(lựa chọn của Claude)*. Mỗi lần xem/rotate ghi audit (`superuser.reveal`, `superuser.rotate`).
- **User ứng dụng** (tên/mật khẩu tự đặt hoặc ngẫu nhiên — mục 13b): `CREATE ROLE … LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE`, là **owner của database** và schema `public` (người dùng chốt: "owner của database") — tự tạo/sửa/xóa bảng, index, schema, extension trusted; đọc ghi dữ liệu. Tạo sau khi container healthy bằng `psql` chạy dưới superuser, SQL đưa qua stdin (mật khẩu không nằm trên dòng lệnh).
- **Restore**: chạy bằng superuser của node mới; object được gán cho user ứng dụng (`pg_restore --no-owner --no-acl --role=<app>`; file SQL thuần được thêm `SET ROLE <app>;` ở đầu luồng).
- Owner và người quản lý thấy connection string của user ứng dụng; connection string superuser chỉ owner/admin.
- **DB tạo ở giai đoạn 1** (một user, là superuser): cột `databases.admin_user` rỗng = "kiểu cũ" — vẫn chạy như trước, UI gắn nhãn "kiểu cũ"; muốn chuyển sang mô hình mới thì backup → restore sang node mới *(lựa chọn của Claude)*.

### 13b-3. Tên nội bộ dạng hash, tên DB theo owner, domain đăng ký trước (người dùng chốt)

- **Tên DB bắt buộc tiền tố `<username owner>_`** (người dùng chốt, giống user ứng dụng): người dùng nhập hậu tố `^[a-z0-9][a-z0-9-]{0,29}$`, UI hiện tiền tố cố định; tên đầy đủ (vd `alice_shop`) dùng để hiển thị, gọi API/CLI và làm tên database bên trong engine. Vì tiền tố là username của chính mình nên tên DB **tự nhiên duy nhất toàn hệ thống** mà không lộ DB của tenant khác (trùng chỉ có thể là với DB của chính mình).
- **Tên nội bộ không hiển thị** lấy từ `node_key = hex(sha256("<username owner>_<tên DB>_<unix nano lúc tạo>"))[:16]`, lưu ở `databases.node_key` (UNIQUE): container + volume `fridge-<engine>-<node_key>`; superuser `su_<node_key>` (thay cho `su_<tên DB>` ở mục 13b-2). Restore vào node mới sinh `node_key` mới.
- **Domain đăng ký trước** (không xác thực DNS — có trường hợp dùng DNS nội bộ): bảng `domains(id, owner_id → users, name UNIQUE, created_at)`. Owner thêm/xóa domain của mình; domain trùng với domain đã có (của bất kỳ ai) → báo chung "domain không đăng ký được" (không nói ai đang dùng). Không xóa được domain đang có DB dùng. Admin thấy mọi domain, xóa được domain không còn dùng *(lựa chọn của Claude)*.
- **Tạo/restore DB**: chọn domain từ danh sách đã đăng ký của **owner** node (dropdown); `databases.domain_id → domains`. Một domain dùng được cho nhiều DB khác engine, mỗi engine một DB (giữ unique `(engine, domain)`) *(lựa chọn của Claude)*.
- **Dữ liệu giai đoạn 1**: lúc tạo admin đầu tiên, domain của các DB cũ được đăng ký cho admin và gắn `domain_id`; DB cũ giữ tên container/volume cũ (`node_key` rỗng = kiểu cũ).
- **Gọi DB bằng tên** (API `{db}`, CLI): dùng tên đầy đủ có tiền tố — luôn duy nhất.
- Audit: `domain.register`, `domain.remove`. CLI: `fridge domains add|ls|rm`. Web: mục **Domains** (bảng của mình; admin thấy tất cả + cột owner).

### 13c. Audit log & Hoạt động (người dùng chốt)

- Bảng `audit_log` **chỉ thêm**: thời điểm, người làm (user, hoặc `system` cho job tự chạy, `recover` cho lệnh khẩn cấp, `anonymous` cho đăng nhập sai user không tồn tại), IP, hành động (mã cố định, vd `db.create`, `auth.login_failed`), đối tượng (loại + id/tên), DB liên quan (nếu có), thành công/thất bại, chi tiết ngắn (JSON). **Không bao giờ** ghi mật khẩu, secret, session/invite token.
- Trigger DB chặn UPDATE; chặn DELETE trừ dòng cũ hơn **1 năm** (job dọn mỗi ngày) — admin cũng không sửa/xóa được lịch sử.
- Sự kiện ghi: setup, đăng nhập (thành công/thất bại), đăng xuất, đổi mật khẩu, lời mời (tạo/nhận), khóa/mở, nâng/hạ quyền, thu hồi phiên; tạo DB, thêm/gỡ người quản lý, **xem mật khẩu DB**, Backup now, tải backup, restore, kiểm tra kỹ; destination tạo/sửa/xóa/dùng chung/test; lịch backup tạo/sửa/xóa; allowlist; pull/prune image; mọi lệnh `fridge recover`; backup theo lịch và xóa do retention (actor `system`).
- **Xem mật khẩu DB thành hành động riêng**: API chi tiết DB trả connection string **đã che mật khẩu**; mật khẩu thật chỉ trả qua một endpoint riêng (nút "Hiện"/"Copy" trên web, `fridge show --reveal` trên CLI) và mỗi lần gọi đều ghi audit.
- **Ai xem** (người dùng chốt): admin xem toàn bộ; user thường xem hành động của chính mình + mọi hành động trên DB mình là owner. Giữ **1 năm**.
- Trang **Audit log**: lọc theo người, hành động, đối tượng/DB, khoảng thời gian; phân trang; **xuất CSV** (theo đúng bộ lọc và quyền). CLI `fridge audit [--user] [--action] [--db] [--since]`.
- Trang **Hoạt động** (admin) — "ai đang làm gì": các job đang chạy (backup, restore, kiểm tra kỹ, pull image) kèm người khởi động + thời điểm; người đang online (phiên có hoạt động trong **5 phút** gần nhất *(lựa chọn của Claude)*); tự làm mới bằng polling.

### 14. Trang Images — quản lý docker image của DB (người dùng chốt)

- **Chỉ admin** thấy (mục sidebar "Images"; API trả 403 cho user).
- **Phạm vi**: chỉ image Fridge dùng cho DB = image trong catalog (vd `postgres:13`…`postgres:18`) ∪ image mà container DB của Fridge (label `fridge.db_id`) đang tham chiếu. Không liệt kê, không pull, không xóa image nào khác trên host (traefik, image của người khác…).
- **Bảng**: tag, digest rút gọn, dung lượng, ngày tạo, **version thật** (từ biến `PG_VERSION` trong config image, vd `18.1`), **số DB đang dùng** (cả DB đang dừng) kèm danh sách tên, **lần dùng gần nhất** (thời điểm tạo/khởi động container DB gần nhất dùng image đó), trạng thái cập nhật.
- **Pull sẵn**: chọn engine + version từ catalog, hoặc "Pull tất cả" → job nền (polling); trạng thái từng tag: chưa có / đang pull / đã có / lỗi.
- **Prune**: màn xác nhận liệt kê đúng các image Fridge **không còn DB nào dùng** + tổng dung lượng giải phóng → xóa. Image đang được DB (kể cả DB dừng) dùng **không bao giờ** bị xóa; kiểm tra lại ngay trước khi xóa (tránh race với tạo DB).
- **Kiểm tra bản mới** (người dùng chốt: vd `postgres:18` đang là 18.1, registry đã có 18.2 → báo "new version"):
  - So digest image local của tag với digest hiện tại của cùng tag trên registry (Docker SDK `DistributionInspect`). Khác nhau → lấy config của manifest mới từ registry (Registry HTTP API v2, token ẩn danh của Docker Hub; stdlib, không thêm thư viện) để đọc `PG_VERSION` mới.
  - Hiển thị huy hiệu **"New version: 18.1 → 18.2"** trên dòng image; sidebar mục Images có chấm báo khi có ít nhất một bản mới.
  - Chạy khi admin bấm "Kiểm tra cập nhật" và tự động **mỗi ngày một lần** trong scheduler *(lựa chọn của Claude)*. Kết quả lưu trong bảng `image_checks(image_ref, local_digest, local_version, remote_digest, remote_version, checked_at, error)`.
  - Server không ra được internet → hiện "không kiểm tra được" kèm lỗi, không chặn gì khác.
  - Chỉ **báo** và cho **pull** bản mới. Áp bản mới cho DB đang chạy (tạo lại container với cùng volume) **để sau** — DB hiện có vẫn chạy image cũ cho tới khi được tạo lại. *(Lựa chọn của Claude; nêu rõ cho người dùng.)*
- API (tên do builder quyết): `GET /api/images`, `POST /api/images/pull`, `GET /api/images/pull/{job}`, `POST /api/images/prune/preview`, `POST /api/images/prune`, `POST /api/images/check-updates`.

## Để builder tự quyết

Tên endpoint/JSON cụ thể, cấu trúc file JS, cách vendor Bootstrap (phiên bản 5.3.x), giao diện chi tiết từng form, cách tổ chức package Go cho auth/web, chi tiết UX CLI trust CA.

## Verification

1. DB metadata rỗng + `FRIDGE_SETUP_TOKEN` → mở `https://localhost:8443` → bị chuyển tới Tạo admin; sai token bị từ chối; đúng → vào Databases. Restart server → không còn màn setup; `POST /api/setup` trả 409.
2. Bắt gói tin (hoặc log request phía server) khi tạo admin và đăng nhập: không có mật khẩu dạng rõ; DB chỉ chứa salt/stored_key/server_key.
3. Test vector SCRAM-SHA-256 của RFC 7677 pass cho phía server (Go) và phía CLI; JS kiểm bằng cùng vector trong một trang test.
4. Sai mật khẩu 5 lần/phút → khóa 5 phút; username không tồn tại trả cùng thông điệp và cùng dạng `server_first`.
5. User A tạo DB `a1` và destination `dA`; user B không thấy `a1`/`dA` (404 qua API, không có trong UI); A thêm B làm người quản lý `a1` → B thấy và backup được, nhưng không thêm người khác được; admin thấy tất cả.
6. Admin tạo destination `dAdmin` (mặc định riêng: B không thấy), rồi đánh dấu `shared` → B chọn được khi đặt lịch, không thấy secret, không duyệt được bucket; chỉ thấy backup của DB mình trên destination đó.
7. Session: đăng xuất/đổi mật khẩu/khóa user → session ID cũ (web và CLI) trả 401 ngay; quá 12h không hoạt động → 401.
8. CSRF: POST bằng cookie với `Origin` lạ → 403.
9. `http://localhost:8080` redirect sang `https://…:8443`; cert là cert tự sinh từ CA nội bộ, có SAN `localhost` và các tên trong `FRIDGE_PUBLIC_HOSTS`.
10. `fridge login` → `fridge ls` chỉ hiện DB của user đó; `fridge logout` → lệnh tiếp theo 401.
11. `docker exec … /fridge recover allowlist 10.0.0.0/8` mở lại truy cập sau khi tự khóa; `recover reset-password` in link dùng được.
12. Sidebar thu gọn/mở, trạng thái giữ sau reload; mọi trang chạy với CSP `default-src 'self'` không lỗi console.
13. Restore một file rỗng / file ảnh đổi đuôi `.dump` / file gzip hỏng header / `.dump` từ PG17 vào node PG16 → 400 ngay với lý do cụ thể, không có container hay bản ghi restore nào được tạo.
14. "Kiểm tra kỹ" một `.dump` bị cắt nửa cuối → `failed` kèm lỗi của pg_restore; một `.sql.gz` cụt → `failed` (lỗi gzip); file nguyên vẹn → `ok` với version nguồn và số mục TOC; container tạm không còn sau khi xong.
15. Images (admin): bảng chỉ có image catalog/DB, cột số DB đúng (tạo 2 DB `postgres:16`, dừng 1 → vẫn đếm 2); "Pull" `postgres:15` → xuất hiện; Prune preview chỉ liệt kê image không DB nào dùng, xóa xong image đang dùng còn nguyên; user thường gọi `/api/images` → 403.
16. Kiểm tra bản mới: pull một tag cũ theo digest rồi gắn lại tag (vd `postgres:18` trỏ về bản 18.0) → "Kiểm tra cập nhật" báo `18.0 → 18.x`; tắt mạng ra ngoài → hiện "không kiểm tra được", trang vẫn dùng bình thường.
