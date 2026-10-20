# Fridge Giai đoạn 2 — Web admin, tài khoản, phân quyền — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Web admin (Vanilla JS + Bootstrap 5) có tài khoản đăng nhập SCRAM, session stateful, vai trò admin/user, sở hữu DB/destination, HTTPS tự ký, validate dump trước restore, cô lập mạng giữa các node, chặn SSRF qua destination và trang quản lý image — CLI dùng cùng session.

**Architecture:** Gói mới `internal/scram` (thuật toán thuần), `internal/auth` (setup, đăng nhập, session, lời mời, user — trạng thái SCRAM/rate-limit trong RAM), `internal/audit` (ghi audit), `internal/dumpcheck` (parse header dump), `internal/registry` (hỏi Docker Hub), `internal/web` (file tĩnh nhúng). `app` thêm khái niệm owner + helper quyền nhận `auth.Principal`, network riêng cho từng node; `objstore` thêm guard SSRF; `api` thêm middleware xác thực và áp quyền trên mọi route. Traefik phục vụ web+API qua `:8443` bằng cert ký từ CA nội bộ.

**Tech Stack:** Go 1.26 stdlib (`crypto/pbkdf2`, `crypto/hmac`, `crypto/sha256`, `embed`, `net/netip`), các dependency sẵn có, Bootstrap 5.3.3 + Bootstrap Icons 1.11.3 (vendored), Web Crypto API. Metadata DB **Postgres 18**.

**Spec:** `docs/superpowers/specs/2026-10-20-fridge-web-admin-design.md` (bổ sung cho `docs/superpowers/specs/2026-10-20-fridge-design.md`).

## Global Constraints

- Mọi lệnh go/git/docker chạy trong **WSL Ubuntu-24.04** tại `/mnt/d/Windows/Desktop/code/Fridge` (xem `env.md` trong workspace SDD). Postgres test: `bash scripts/test-db.sh &&` trước lệnh test (container `fridge-test-pg`, image **`postgres:18`** từ Task 1). Commit message LF.
- **Metadata DB = Postgres 18**: `fridge-meta` chạy `postgres:18`, volume mount ở `/var/lib/postgresql` (image 18 dời PGDATA; mount `/var/lib/postgresql/data` là sai với 18). Postgres dùng cho test cũng là 18.
- **Không thêm dependency Go mới.** Dùng stdlib + các module đã có trong `go.mod`.
- Frontend: **Vanilla JS (ES modules) + Bootstrap 5.3.3 + Bootstrap Icons 1.11.3 vendored** trong `internal/web/static/vendor/`; không CDN, không npm, không build step; **không inline script/style** (CSP `default-src 'self'`).
- SCRAM-SHA-256 theo RFC 5802/7677, chỉ `n,,` (không channel binding). PBKDF2 **600000** vòng khi đăng ký; server nhận `iterations` trong [600000, 10000000], salt đúng **16 byte**.
- **Username tài khoản Fridge: `^[a-z][a-z0-9]{2,15}$`** (3–16 ký tự, chữ thường + số; **không `_`**, `.`, `-`). Kiểm ở `auth.Setup`, `auth.CreateInvite`, `auth.AcceptInvite` (→ `auth.ErrInvalid`) **và** bằng CHECK trên `users.username`, `invites.username`. Ví dụ hợp lệ trong test: `alice`, `bob`, `carol` (không dùng `a`/`b`). Mật khẩu ≥ **12** ký tự (kiểm ở client).
- **Quy tắc tiền tố** (mọi engine): tên DB và tên user ứng dụng = `<username owner>_<hậu tố>`; dấu `_` đầu tiên luôn là ranh giới (username không có `_`). Hậu tố tên DB `^[a-z0-9][a-z0-9-]{0,29}$` (không `_`); hậu tố user ứng dụng `^[a-z0-9_]{1,15}$` (tổng ≤ 32 ký tự, giới hạn MySQL). Client gửi kèm sẵn đúng tiền tố của chính owner → bỏ tiền tố rồi kiểm. Trùng tên chỉ kiểm trong phạm vi owner. Superuser node: `su_<node_key>`.
- Session ID 32 byte ngẫu nhiên (base64url, không padding); DB chỉ lưu **SHA-256**. Cookie `fridge_session`: `HttpOnly; Secure; SameSite=Strict; Path=/`. Hết hạn: không hoạt động **12h**, tuyệt đối **7 ngày**; cập nhật `last_seen_at` tối đa 1 lần/phút; dọn session hết hạn mỗi giờ. CLI: env **`FRIDGE_SESSION`** ghi đè file `~/.config/fridge/session`.
- Khóa đăng nhập: **5** lần sai trong **1 phút** theo (username, IP) → khóa **5 phút**; re-auth dùng **chung** bộ đếm. Trạng thái SCRAM dở dang sống **60s**. Lời mời hết hạn **48h**, dùng một lần; token lời mời chỉ đi trong **body** (`POST /api/invites/lookup|accept`), không bao giờ trong đường dẫn URL. Reauth token sống **2 phút**, dùng một lần.
- Tài nguyên không có quyền xem → **404**; thao tác cấm trên tài nguyên thấy được → **403**; trước khi setup → `/api/*` (trừ route công khai) trả **401 `{"error":"setup required"}`**.
- Cổng: Traefik `websecure :8443` (TLS, web + API), `web :8080` (chỉ redirect sang HTTPS :8443), `pg :5432`, `ping :8082`. Fridge vẫn nghe HTTP nội bộ `:8080`, không publish.
- **Cô lập mạng** (từ Task 8): `fridge-net` chỉ có `traefik`, `fridge`, `fridge-meta` (+ `minio` profile dev). Mỗi node DB có network riêng **`fridge-node-<node_key>`** (DB kiểu cũ: `fridge-node-legacy-<db id>`), tạo với `Internal: true`; chỉ gắn container DB đó (alias = tên container) + container Traefik (tên lấy từ env `FRIDGE_TRAEFIK_CONTAINER`, mặc định `fridge-traefik`). Backend route Traefik giữ `<tên container>:<port>`. Container tạm (kiểm tra kỹ dump) chạy `NoNetwork`.
- **Chặn SSRF** (từ Task 12): endpoint destination do user **không phải admin** tạo/sửa/kết nối không được tới loopback, link-local (`169.254.0.0/16`, `fe80::/10`), unspecified, subnet của `fridge-net` và mọi `fridge-node-*`, hay tên `fridge`, `fridge-meta`, `traefik`, `fridge-traefik`; RFC1918 khác vẫn được. Kiểm lúc tạo/sửa **và** lúc dial (`net.Dialer.Control`). Setting `s3_endpoint_allowlist` (host) bỏ qua guard. Lỗi chung: `"endpoint không được phép"`.
- **Proxy tin cậy**: chỉ tin `X-Forwarded-For` khi peer thuộc **subnet của `fridge-net`** (lấy qua Docker API lúc khởi động) hoặc loopback; env `FRIDGE_TRUSTED_PROXY` = danh sách CIDR (phẩy) ghi đè mặc định.
- Env server: **bỏ `FRIDGE_TOKEN`** hoàn toàn; thêm `FRIDGE_SETUP_TOKEN` (cần khi chưa có user), `FRIDGE_PUBLIC_HOSTS` (tùy chọn, phẩy), `FRIDGE_TRUSTED_PROXY` (tùy chọn, CIDR), `FRIDGE_TRAEFIK_CONTAINER` (mặc định `fridge-traefik`). Env CLI: `FRIDGE_URL` (mặc định `https://localhost:8443`), `FRIDGE_CA_FILE`, `FRIDGE_SESSION`.
- Scheduler: job cố định đăng ký bằng `Scheduler.AddFixed(spec, fn)` (Task 9); `Reload` chỉ gỡ các entry của lịch backup, **không bao giờ** gỡ job cố định.
- Audit (từ Task 11): mọi route/lệnh tạo sau Task 11 ghi audit qua `audit.Logger.Record` với đúng mã hành động liệt kê ở Task 11; detail không bao giờ chứa mật khẩu/secret/token.
- Thao tác nền dùng **polling 2–3s** (không SSE). Store: chỉ thêm **migration 4** (không sửa migration 1–3).

## Review Focus

1. **Quyền bị vượt qua đường gián tiếp** — restore theo key một bản backup của DB người khác trên destination dùng chung; tải backup của DB không có quyền; đặt/sửa/xóa lịch của DB người khác; đặt lịch dùng destination riêng của người khác; "Kiểm tra kỹ" file của người khác; người quản lý (không phải owner) thêm member; Backup now rơi vào destination không thuộc owner → đều phải bị chặn. Test ở Task 10 (`TestIndirectAccessBlocked`) và Task 5 (`TestBackupNowFallbackOwnerDestination`).
2. **Session ID / token lộ ra ngoài** — không được xuất hiện trong body JSON trả cho web, trong URL (kể cả invite token), trong log; DB chỉ chứa hash. Test ở Task 4 (`TestSessionStoredHashed`) và Task 9 (`TestWebLoginNoTokenInBody`, `TestInviteTokenNotInPath`).
3. **Setup chạy song song / chạy lại** — hai request setup đồng thời chỉ tạo đúng một admin; setup sau khi đã có user → 409. Test ở Task 1 (`TestCreateFirstAdminOnce`) và Task 4 (`TestSetup`).
4. **Tự khóa admin** — khóa/hạ quyền admin hoạt động cuối cùng bị từ chối, kể cả khi hai admin hạ nhau đồng thời; user bị khóa mất mọi session ngay; `fridge recover promote` cứu được. Test ở Task 1 (`TestLastAdminConcurrent`), Task 4 (`TestLastAdminGuard`) và Task 17 (`TestRecoverResetAndPromote`).
5. **Giả mạo IP qua `X-Forwarded-For`** — request không đến từ subnet proxy tin cậy mà gửi XFF thì XFF bị bỏ qua. Test ở Task 9 (`TestClientIPTrustsOnlyProxy`).
6. **Thoát khỏi container DB** — shell trong node A không tới được node B, `fridge-meta`, API Fridge. Test ở Task 8 (`TestNodeNetworkIsolation`, integration).
7. **SSRF qua destination** — user thường không trỏ endpoint vào hạ tầng nội bộ, kể cả bằng DNS rebinding. Test ở Task 12 (`TestGuardCheckIP`, `TestDialGuardBlocksAtConnect`, `TestDestinationSSRFAPI`).

---

## File Structure

```
scripts/test-db.sh                      # postgres:18 (Task 1)
internal/store/store.go                 # + migration 4, migrateTo
internal/store/export_test.go           # OpenRaw, MigrateTo cho test (mới)
internal/store/storetest/storetest.go   # + NewEmpty
internal/store/users.go                 # users, invites, sessions (mới)
internal/store/databases.go             # + OwnerID, OwnerName, members, visible queries, domains
internal/store/destinations.go          # + OwnerID, OwnerName, Shared, DeletedAt, visible queries
internal/store/jobs.go                  # + StartedByID, backups-by-dest/key, LastBackups, FailRunning dump_checks
internal/store/checks.go                # dump_checks, image_checks (mới)
internal/store/audit.go                 # audit_log, RunningJobs, OnlineSessions (mới)
internal/scram/scram.go                 # RFC 5802/7677 (mới)
internal/auth/{auth.go,login.go,sessions.go,users.go}
internal/audit/audit.go                 # Logger (mới)
internal/engine/{engine.go,postgres.go} # hai user, RestoreCmd mới
internal/container/docker.go            # RunSpec Network/Cmd/NoNetwork
internal/container/network.go           # network node, subnets (mới)
internal/container/images.go            # image ops Docker (mới)
internal/objstore/guard.go              # guard SSRF (mới)
internal/app/access.go                  # helper quyền theo Principal (mới)
internal/app/members.go                 # members DB (mới)
internal/app/names.go                   # tiền tố tên DB/user, credentials (mới)
internal/app/superuser.go               # Reveal/RotateSuperuser (mới)
internal/app/network.go                 # network node, ReconcileNetworks (mới)
internal/app/endpoint.go                # CheckEndpoint, endpoint allowlist (mới)
internal/app/scheduler.go               # AddFixed
internal/dumpcheck/dumpcheck.go         # header inspect (mới) + testdata/
internal/app/dumpchecks.go              # job "kiểm tra kỹ" (mới)
internal/registry/registry.go           # Docker Hub remote version (mới)
internal/app/images.go                  # trang Images (mới)
internal/api/middleware.go              # principal, CSRF, client IP, security headers (mới)
internal/api/auth_routes.go             # route xác thực/user/session (mới)
internal/api/api.go                     # áp quyền route cũ
internal/api/admin_routes.go            # members, share, domains, superuser, dump checks, images, endpoint allowlist (mới)
internal/api/audit_routes.go            # audit, activity (mới)
internal/pki/ca.go, internal/proxy/traefik.go
internal/web/web.go + static/**         # web admin (mới)
cmd/fridge/{main.go,cli.go,login.go,recover.go,webcert.go,resources.go}
deploy/{docker-compose.yml,traefik.yml}, deploy/.env.example (mới), README.md
scripts/ui-smoke/{smoke.mjs,run.sh}     # smoke UI Playwright trong Docker (mới)
```

---

### Task 1: Store — migration 4, users, invites, sessions

**Files:**
- Modify: `internal/store/store.go` (thêm migration 4; tách `migrate` thành `migrateTo(ctx, n int)`), `internal/store/storetest/storetest.go` (thêm `NewEmpty`), `scripts/test-db.sh`
- Create: `internal/store/users.go`, `internal/store/users_test.go`, `internal/store/export_test.go`

**Interfaces:**
- `scripts/test-db.sh`: image **`postgres:18`**; nếu container `fridge-test-pg` đã có nhưng chạy image khác `postgres:18` → `docker rm -f fridge-test-pg` rồi tạo lại (chỉ là dữ liệu test). Cổng `55432`, mật khẩu `postgres`, `FRIDGE_TEST_DATABASE_URL` mặc định giữ nguyên.
- Test helper migration:
  - `internal/store/export_test.go` (package `store`): `func OpenRaw(ctx context.Context, url string) (*Store, error)` (mở pool, **không** chạy migration) và `func (s *Store) MigrateTo(ctx context.Context, n int) error` (gọi `migrateTo`, áp các migration 1..n).
  - `storetest.NewEmpty(t *testing.T) string` — tạo database trống `fridge_t_<hex>` (không migration), drop khi cleanup, trả URL.
- Produces (migration 4 tạo **toàn bộ** schema giai đoạn 2, các task sau chỉ thêm method):
  - `users(id BIGINT identity PK, username TEXT UNIQUE NOT NULL CHECK (username ~ '^[a-z][a-z0-9]{2,15}$'), role TEXT NOT NULL CHECK (role IN ('admin','user')), scram_salt BYTEA NOT NULL, scram_iterations INT NOT NULL, stored_key BYTEA NOT NULL, server_key BYTEA NOT NULL, disabled BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), created_by BIGINT NULL REFERENCES users(id))`
  - `invites(id identity PK, token_hash BYTEA UNIQUE NOT NULL, username TEXT NOT NULL CHECK (username ~ '^[a-z][a-z0-9]{2,15}$'), role TEXT NOT NULL, purpose TEXT NOT NULL CHECK (purpose IN ('new_user','reset_password')), user_id BIGINT NULL REFERENCES users(id) ON DELETE CASCADE, created_by BIGINT NULL REFERENCES users(id), expires_at TIMESTAMPTZ NOT NULL, used_at TIMESTAMPTZ NULL, created_at …)`
  - `sessions(id_hash BYTEA PRIMARY KEY, user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE, created_at, last_seen_at, expires_at TIMESTAMPTZ NOT NULL, ip TEXT NOT NULL DEFAULT '', user_agent TEXT NOT NULL DEFAULT '')`
  - `ALTER TABLE databases ADD owner_id BIGINT NULL REFERENCES users(id)`; `database_members(database_id BIGINT REFERENCES databases(id) ON DELETE CASCADE, user_id BIGINT REFERENCES users(id) ON DELETE CASCADE, added_by BIGINT NULL REFERENCES users(id), added_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (database_id, user_id))`
  - `ALTER TABLE destinations ADD owner_id BIGINT NULL REFERENCES users(id), ADD shared BOOLEAN NOT NULL DEFAULT false`
  - `ALTER TABLE restores ADD started_by_id BIGINT NULL REFERENCES users(id)`; `ALTER TABLE backups ADD started_by_id BIGINT NULL REFERENCES users(id)` (NULL = hệ thống/lịch)
  - `dump_checks(id identity PK, destination_id BIGINT NOT NULL, s3_key TEXT NOT NULL, status TEXT NOT NULL, result JSONB NOT NULL DEFAULT '{}', log TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', started_by_id BIGINT NULL REFERENCES users(id), started_at, finished_at NULL)`
  - `image_checks(image_ref TEXT PRIMARY KEY, local_digest, local_version, remote_digest, remote_version, error TEXT NOT NULL DEFAULT '', checked_at TIMESTAMPTZ NOT NULL)`
  - **Ràng buộc toàn vẹn** (spec mục 1b), cùng migration 4, theo đúng thứ tự:
    1. `destinations` thêm `deleted_at TIMESTAMPTZ NULL`; bỏ `UNIQUE(name)` cũ; tạo `CREATE UNIQUE INDEX destinations_owner_name ON destinations (COALESCE(owner_id, 0), name) WHERE deleted_at IS NULL`.
    2. `backups.destination_id`, `restores.destination_id` → `BIGINT`.
    3. Tombstone: với mỗi `destination_id` trong `backups ∪ restores` không còn trong `destinations` → `INSERT … OVERRIDING SYSTEM VALUE` (id cũ, name `'_deleted_'||id`, provider `'deleted'`, `deleted_at = now()`).
    4. `UPDATE backups SET schedule_id = NULL WHERE schedule_id NOT IN (SELECT id FROM backup_schedules)`.
    5. FK: `backups.destination_id`, `restores.destination_id`, `dump_checks.destination_id` → `destinations(id)`; `backups.schedule_id` → `backup_schedules(id) ON DELETE SET NULL`.
    6. CHECK:
       - `databases`: `status IN ('creating','running','stopped','error','deleted')`, `engine IN ('postgres','mysql','mariadb','mongo','redis')`, `tls_mode IN ('internal','acme')`.
       - `backups`: `status IN ('running','ok','failed')`, `trigger IN ('schedule','manual','pre-delete','pre-restore')`, `size_bytes >= 0`, `(status = 'running') = (finished_at IS NULL)`, `status <> 'ok' OR s3_key <> ''`.
       - `restores`: `mode IN ('new','overwrite')`, `status IN ('running','ok','failed')`, `(status = 'running') = (finished_at IS NULL)`.
       - `dump_checks`: `status IN ('running','ok','failed')`, `(status = 'running') = (finished_at IS NULL)`.
       - `backup_schedules`: `keep_latest >= 1`, `cron <> ''`.
       - `users`: `octet_length(scram_salt) = 16`, `octet_length(stored_key) = 32`, `octet_length(server_key) = 32`, `scram_iterations BETWEEN 600000 AND 10000000` (+ CHECK username ở trên).
       - `invites`: `role IN ('admin','user')`, `expires_at > created_at`, `(purpose = 'reset_password') = (user_id IS NOT NULL)` (+ CHECK username ở trên).
       - `sessions`: `expires_at > created_at`.
    6a. `domains(id BIGINT identity PK, owner_id BIGINT NULL REFERENCES users(id), name TEXT NOT NULL UNIQUE, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`; `ALTER TABLE databases ADD admin_user TEXT NOT NULL DEFAULT '', ADD admin_password_enc TEXT NOT NULL DEFAULT '', ADD node_key TEXT NOT NULL DEFAULT '', ADD domain_id BIGINT NULL REFERENCES domains(id) ON DELETE SET NULL` (admin_user/node_key rỗng = DB kiểu cũ giai đoạn 1) + `CHECK (admin_user = '' OR admin_user ~ '^su_[0-9a-f]{16}$')`, `CHECK (node_key = '' OR node_key ~ '^[0-9a-f]{16}$')`, `CREATE UNIQUE INDEX db_node_key ON databases(node_key) WHERE node_key <> ''`.
    6b. `audit_log(id BIGINT identity PK, at TIMESTAMPTZ NOT NULL DEFAULT now(), actor_id BIGINT NULL REFERENCES users(id), actor TEXT NOT NULL, ip TEXT NOT NULL DEFAULT '', action TEXT NOT NULL CHECK (action ~ '^[a-z_]+\.[a-z_]+$'), target_type TEXT NOT NULL DEFAULT '', target_id TEXT NOT NULL DEFAULT '', db_id BIGINT NULL REFERENCES databases(id), success BOOLEAN NOT NULL, detail JSONB NOT NULL DEFAULT '{}')` + index `(at DESC)`, `(actor_id, at DESC)`, `(db_id, at DESC)`; trigger `BEFORE UPDATE OR DELETE ON audit_log`: UPDATE luôn `RAISE` (P0001); DELETE chỉ cho phép khi `OLD.at < now() - interval '365 days'`.
    7. Trigger `BEFORE INSERT OR UPDATE ON database_members`: `RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'owner cannot be a member'` khi `user_id` = `databases.owner_id`.
  - `CreateFirstAdmin` (một transaction + `pg_advisory_xact_lock`), theo thứ tự: tạo user `role='admin'` → gán `owner_id` NULL của databases & destinations cho admin → đăng ký domain của DB cũ: `INSERT INTO domains(owner_id, name) SELECT DISTINCT $admin, domain FROM databases WHERE domain <> '' ON CONFLICT (name) DO NOTHING` → `UPDATE databases d SET domain_id = dm.id FROM domains dm WHERE dm.name = d.domain AND d.domain_id IS NULL` → `ALTER TABLE databases ALTER owner_id SET NOT NULL; ALTER TABLE destinations ALTER owner_id SET NOT NULL; ALTER TABLE domains ALTER owner_id SET NOT NULL`.
  - `mapErr`: thêm sentinel `ErrInvalid` cho SQLSTATE `23514` (check) và `P0001` (trigger). Thêm sentinel `ErrLastAdmin`.
  - **Chặn admin cuối cùng ở tầng store**: `SetUserRole` và `SetUserDisabled` chạy trong một transaction có `pg_advisory_xact_lock(hashtext('fridge.admins'))`; nếu sau thay đổi số admin hoạt động (`role='admin' AND NOT disabled`) = 0 → rollback, trả `ErrLastAdmin`.

```go
type User struct { ID int64; Username, Role string; Salt []byte; Iterations int; StoredKey, ServerKey []byte; Disabled bool; CreatedAt time.Time; CreatedBy *int64 }
type Invite struct { ID int64; TokenHash []byte; Username, Role, Purpose string; UserID, CreatedBy *int64; ExpiresAt time.Time; UsedAt *time.Time; CreatedAt time.Time }
type Session struct { IDHash []byte; UserID int64; CreatedAt, LastSeenAt, ExpiresAt time.Time; IP, UserAgent string }
var ErrInvalid, ErrLastAdmin error

func (s *Store) CountUsers(ctx) (int, error)
func (s *Store) CreateFirstAdmin(ctx, u *User) error   // xem trên; ErrConflict nếu đã có user
func (s *Store) CreateUser(ctx, u *User) error          // ErrConflict nếu trùng username; username sai CHECK → ErrInvalid
func (s *Store) GetUser(ctx, id int64) (User, error)
func (s *Store) GetUserByName(ctx, name string) (User, error)
func (s *Store) ListUsers(ctx) ([]User, error)
func (s *Store) UpdateUserCredentials(ctx, id int64, salt []byte, iter int, stored, server []byte) error
func (s *Store) SetUserRole(ctx, id int64, role string) error          // guarded → ErrLastAdmin
func (s *Store) SetUserDisabled(ctx, id int64, disabled bool) error    // guarded → ErrLastAdmin
func (s *Store) CountActiveAdmins(ctx) (int, error)
func (s *Store) CreateInvite(ctx, inv *Invite) error
func (s *Store) GetInviteByHash(ctx, hash []byte) (Invite, error)
func (s *Store) MarkInviteUsed(ctx, id int64) error     // chỉ khi used_at IS NULL, ngược lại ErrConflict
func (s *Store) CreateSession(ctx, x *Session) error
func (s *Store) GetSession(ctx, hash []byte) (Session, error)
func (s *Store) TouchSession(ctx, hash []byte, at time.Time) error
func (s *Store) DeleteSession(ctx, hash []byte) error
func (s *Store) DeleteUserSessions(ctx, userID int64, except []byte) error  // except nil = xóa hết
func (s *Store) ListSessions(ctx, userID *int64) ([]Session, error)        // nil = mọi user; mới nhất trước
func (s *Store) DeleteExpiredSessions(ctx, now time.Time, idle time.Duration) (int, error) // expires_at < now OR last_seen_at < now-idle
```
`FailRunning` (đã có) cập nhật thêm `dump_checks` đang `running`.

- [ ] **Step 1: Viết test** (package `store_test`, `storetest.New`; username trong test luôn hợp lệ: `alice`, `bob`, `carol`)

```go
func TestMetaPostgres18(t *testing.T)         // SHOW server_version → chuỗi bắt đầu bằng "18"
func TestMigration4Applied(t *testing.T)      // schema_migrations count == 4; cột databases.owner_id, destinations.shared, backups.started_by_id tồn tại (information_schema)
func TestCreateFirstAdminOnce(t *testing.T)   // 2 goroutine CreateFirstAdmin đồng thời → đúng 1 nil, 1 ErrConflict; CountUsers==1; DB/destination có sẵn (owner NULL) giờ owner = admin
func TestSetupBackfillsDomains(t *testing.T)  // 2 DB cũ cùng domain "old.com" (1 status error, 1 running) + 1 DB "other.com", owner NULL → CreateFirstAdmin không lỗi; domains có đúng 2 dòng ("old.com","other.com") owner = admin; databases.domain_id của cả 3 gắn đúng; domains.owner_id NOT NULL (insert NULL → lỗi)
func TestUserCRUD(t *testing.T)               // CreateUser trùng → ErrConflict; SetUserRole/Disabled; CountActiveAdmins bỏ qua admin disabled
func TestUsernameCheck(t *testing.T)          // CreateUser "alice_x", "Al", "ab", "a-b", "1abc", 17 ký tự → ErrInvalid; "alice", "bob2" OK; CreateInvite username "bob_1" → ErrInvalid
func TestLastAdminConcurrent(t *testing.T)    // 2 admin (alice, bob): 2 goroutine đồng thời SetUserRole(alice,"user") và SetUserRole(bob,"user") → đúng 1 nil, 1 ErrLastAdmin; CountActiveAdmins == 1; 1 admin: SetUserDisabled(chính nó, true) → ErrLastAdmin
func TestInviteSingleUse(t *testing.T)        // MarkInviteUsed lần 2 → ErrConflict
func TestSessionLifecycle(t *testing.T)       // Create/Get/Touch; DeleteUserSessions(except=h1) giữ h1 xóa h2; DeleteExpiredSessions xóa đúng row quá idle và quá expires_at
func TestFailRunningIncludesDumpChecks(t *testing.T)
func TestMigration4Tombstones(t *testing.T)   // url := storetest.NewEmpty(t); s := store.OpenRaw(url); s.MigrateTo(ctx, 3); chèn backup trỏ destination_id 99 (không tồn tại) + schedule_id 77 mồ côi; s.MigrateTo(ctx, 4) → có destination id 99 deleted_at != NULL; backup.schedule_id NULL; FK tồn tại
func TestIntegrityConstraints(t *testing.T)   // bảng: insert backup status "weird" → ErrInvalid; backup ok với s3_key "" → ErrInvalid; schedule keep_latest 0 → ErrInvalid; user salt 15 byte → ErrInvalid; invite reset không user_id → ErrInvalid; thêm owner làm member → ErrInvalid; backup trỏ destination không tồn tại → lỗi FK
func TestOwnerNotNullAfterSetup(t *testing.T) // sau CreateFirstAdmin: INSERT databases với owner_id NULL → lỗi
```
- [ ] **Step 2:** `bash scripts/test-db.sh && go test ./internal/store/...` → FAIL (script đã đổi sang `postgres:18` trước khi chạy).
- [ ] **Step 3: Implement.** Cập nhật assertion đếm `schema_migrations` hiện có thành 4.
- [ ] **Step 4:** chạy lại → PASS.
- [ ] **Step 5: Commit** `feat(store): users, invites, sessions and phase-2 schema`.

---

### Task 2: Store — owner, members, destination dùng chung, domains, checks

**Files:**
- Modify: `internal/store/databases.go`, `internal/store/destinations.go`, `internal/store/jobs.go`
- Create: `internal/store/checks.go`, tests tương ứng

**Interfaces:**
- Consumes: migration 4 (Task 1).
- Produces:

```go
// Database thêm: OwnerID *int64; OwnerName string (LEFT JOIN users, chỉ đọc); AdminUser, AdminPasswordEnc, NodeKey string; DomainID *int64. CreateDatabase ghi owner_id, admin_user, admin_password_enc, node_key, domain_id.
func (s *Store) ListDatabasesVisible(ctx, userID int64) ([]Database, error) // owner hoặc member; bỏ status deleted
func (s *Store) IsMember(ctx, dbID, userID int64) (bool, error)
type Member struct { UserID int64; Username string; AddedAt time.Time }
func (s *Store) AddMember(ctx, dbID, userID int64, addedBy *int64) error   // trùng → ErrConflict
func (s *Store) RemoveMember(ctx, dbID, userID int64) error                // không có → ErrNotFound
func (s *Store) ListMembers(ctx, dbID int64) ([]Member, error)
func (s *Store) SetDatabaseAdminPassword(ctx, id int64, passwordEnc string) error
// Destination thêm: OwnerID *int64; OwnerName string; Shared bool; DeletedAt *time.Time. CreateDestination ghi owner_id, shared.
// GetDestination trả cả bản ghi đã xóa; ListDestinations / ListDestinationsVisible bỏ bản ghi đã xóa.
func (s *Store) ListDestinationsVisible(ctx, userID int64) ([]Destination, error) // (owner_id=user OR shared) AND deleted_at IS NULL; theo id tăng dần
func (s *Store) SetDestinationShared(ctx, id int64, shared bool) error
func (s *Store) DeleteDestination(ctx, id int64) error // ĐỔI: xóa mềm — còn lịch trỏ tới → ErrInUse; ngược lại deleted_at=now(), secret_key_enc='', access_key='', shared=false; đã xóa → ErrNotFound
// Backup thêm StartedByID *int64 (StartBackup ghi). Restore thêm StartedByID *int64 (StartRestore ghi).
func (s *Store) GetBackupByKey(ctx, destID int64, key string) (Backup, error)
func (s *Store) ListOKBackupsOnDestination(ctx, destID int64, visibleTo *int64) ([]Backup, error) // nil = tất cả; ngược lại chỉ DB user thấy được (owner/member); mới nhất trước
type DumpCheck struct { ID, DestinationID int64; S3Key, Status, Log, Error string; Result map[string]any; StartedByID *int64; StartedAt time.Time; FinishedAt *time.Time }
func (s *Store) StartDumpCheck(ctx, c *DumpCheck) error
func (s *Store) FinishDumpCheck(ctx, id int64, status string, result map[string]any, log, errText string) error
func (s *Store) GetDumpCheck(ctx, id int64) (DumpCheck, error)
type Domain struct { ID int64; OwnerID int64; OwnerName, Name string; CreatedAt time.Time; InUse int /* số DB đang hoạt động (status NOT IN ('error','deleted')) dùng */ }
func (s *Store) CreateDomain(ctx, d *Domain) error                       // trùng tên (của bất kỳ ai) → ErrConflict
func (s *Store) ListDomains(ctx, ownerID *int64) ([]Domain, error)       // nil = tất cả
func (s *Store) GetDomain(ctx, id int64) (Domain, error)
func (s *Store) GetDomainByName(ctx, name string) (Domain, error)
func (s *Store) DeleteDomain(ctx, id int64) error                        // còn DB hoạt động (status NOT IN ('error','deleted')) dùng → ErrInUse; DB error/deleted không chặn (domain_id của chúng → NULL nhờ ON DELETE SET NULL)
type ImageCheck struct { ImageRef, LocalDigest, LocalVersion, RemoteDigest, RemoteVersion, Error string; CheckedAt time.Time }
func (s *Store) UpsertImageCheck(ctx, c ImageCheck) error
func (s *Store) ListImageChecks(ctx) ([]ImageCheck, error)
```

- [ ] **Step 1: Viết test** (users `alice`, `bob`, admin `root1` tạo bằng `CreateFirstAdmin`/`CreateUser`)

```go
func TestVisibleDatabases(t *testing.T)    // alice owner alice_one, bob owner bob_two, alice member của bob_two → alice thấy [alice_one, bob_two]; bob thấy [bob_two]; OwnerName đúng
func TestMembers(t *testing.T)             // Add trùng → ErrConflict; Remove không có → ErrNotFound; ListMembers có Username
func TestVisibleDestinations(t *testing.T) // alice dA (riêng), admin dS (shared=true), admin dP (riêng) → alice thấy [dA, dS]
func TestDestinationSoftDelete(t *testing.T) // còn schedule → ErrInUse; xóa lịch rồi xóa → không còn trong List/Visible, GetDestination vẫn có (DeletedAt != nil, secret rỗng); backups cũ vẫn JOIN được; tạo lại cùng owner + cùng tên → OK; owner khác cùng tên → OK, cùng owner trùng tên khi chưa xóa → ErrConflict
func TestBackupsOnDestination(t *testing.T)// backup ok của alice_one và bob_two trên dS → visibleTo=&alice chỉ trả alice_one; nil trả 2; GetBackupByKey tìm đúng, key lạ → ErrNotFound
func TestBackupStartedBy(t *testing.T)     // StartBackup với StartedByID=&alice → GetBackup trả StartedByID == alice; nil → nil
func TestDumpAndImageChecks(t *testing.T)  // Start/Finish/Get dump check (result JSON round-trip); Upsert image check 2 lần → 1 row, giá trị mới
func TestDatabaseAdminUser(t *testing.T)   // admin_user "su-bad!" → ErrInvalid; node_key không phải 16 hex → ErrInvalid; hai DB trùng node_key → ErrConflict; SetDatabaseAdminPassword cập nhật
func TestDomains(t *testing.T)             // alice đăng ký "alice.lan"; bob đăng ký "alice.lan" → ErrConflict; ListDomains(&alice) chỉ của alice, InUse đếm đúng (DB status error không đếm); xóa domain đang có DB running → ErrInUse; domain chỉ còn DB status error → DeleteDomain OK và databases.domain_id của DB đó thành NULL
func TestDBNameGlobalUnique(t *testing.T)  // index db_name giữ nguyên: "alice_shop" trùng khi bản cũ đang chạy → ErrConflict; "alice_shop" và "bob_shop" cùng tồn tại → OK
```
- [ ] **Step 2:** `bash scripts/test-db.sh && go test ./internal/store/...` → FAIL. **Step 3:** implement. **Step 4:** PASS.
- [ ] **Step 5: Commit** `feat(store): ownership, members, shared destinations, domains, checks`.

---

### Task 3: Gói `scram` (RFC 5802 / 7677)

**Files:** Create `internal/scram/scram.go`, `internal/scram/scram_test.go`

**Interfaces:**
```go
func SaltedPassword(password string, salt []byte, iter int) []byte          // PBKDF2-HMAC-SHA-256, 32 byte
func Keys(salted []byte) (storedKey, serverKey []byte)                      // StoredKey = SHA256(HMAC(salted,"Client Key")); ServerKey = HMAC(salted,"Server Key")
type Server struct { ClientFirstBare, ServerFirst, Nonce string }
func StartServer(clientFirst string, salt []byte, iter int, serverNonce string) (srv *Server, username string, err error) // chỉ chấp nhận gs2 "n,,"
func (s *Server) Finish(clientFinal string, storedKey, serverKey []byte) (serverFinal string, err error)                    // ErrProof nếu sai; so sánh constant-time; kiểm nonce & c=biws
type Client struct{ /* … */ }
func NewClient(username, password, clientNonce string) *Client
func (c *Client) First() string                                  // "n,,n=<user>,r=<nonce>"
func (c *Client) Final(serverFirst string) (string, error)       // tính SaltedPassword từ s=,i= của server; lỗi nếu nonce không bắt đầu bằng client nonce
func (c *Client) FinalWithSalted(serverFirst string, salted []byte) (string, error) // dùng khi đã có salted (không cần password)
func (c *Client) Verify(serverFinal string) error                // kiểm v= ServerSignature
var ErrProof, ErrFormat error
```

- [ ] **Step 1: Viết test** — vector RFC 7677 (giữ nguyên chuỗi):

```go
func TestRFC7677Vector(t *testing.T) {
	// user "user", password "pencil", client nonce "rOprNGfwEbeRWgbNEkqO"
	// server-first: "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	// client-final: "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	// server-final: "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
	// Client: First()=="n,,n=user,r=rOprNGfwEbeRWgbNEkqO"; Final(serverFirst)==client-final; Verify(server-final)==nil
	// Server: StartServer(client.First(), salt(base64 W22Z…), 4096, "%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0") → ServerFirst == server-first, username "user"; Finish(client-final, Keys(SaltedPassword("pencil",salt,4096))) == server-final
}
func TestWrongPassword(t *testing.T)   // client password "pencil2" → Server.Finish trả ErrProof
func TestRejectChannelBinding(t *testing.T) // client-first "p=tls-unique,,n=user,r=x" và "y,,…" → ErrFormat
func TestTamperedNonce(t *testing.T)  // client-final với r= khác → ErrProof hoặc ErrFormat
```
- [ ] **Step 2:** `go test ./internal/scram/` → FAIL. **Step 3:** implement (`crypto/pbkdf2.Key(sha256.New, password, salt, iter, 32)`). **Step 4:** PASS.
- [ ] **Step 5: Commit** `feat(scram): RFC 5802/7677 SCRAM-SHA-256`.

---

### Task 4: Gói `auth` — setup, đăng nhập, session, lời mời, user

**Files:** Create `internal/auth/{auth.go,login.go,sessions.go,users.go}` + `*_test.go`

**Interfaces:**
- Consumes: store (Task 1), scram (Task 3).
- Produces:

```go
type Principal struct { UserID int64; Username, Role string; SessionHash []byte }
func (p Principal) IsAdmin() bool
type Credentials struct { Salt []byte; Iterations int; StoredKey, ServerKey []byte } // JSON base64 ở tầng API
func (c Credentials) Validate() error  // salt 16 byte, iter ∈ [600000,10000000], key 32 byte → ErrInvalid
func ValidUsername(name string) bool   // ^[a-z][a-z0-9]{2,15}$
type Attempt struct { Username string; UserID *int64 /* nil = user không tồn tại */ } // cho audit auth.login_failed (Task 11)
type Service struct { Store *store.Store; Secret []byte; SetupToken string; Now func() time.Time /* nil=time.Now */ ; /* RAM state */ }
func New(st *store.Store, secret []byte, setupToken string) *Service
var ErrInvalid, ErrSetupDone, ErrBadSetupToken, ErrBadCredentials, ErrLocked, ErrUnauthenticated, ErrForbidden, ErrLastAdmin, ErrInviteInvalid error

func (s *Service) SetupNeeded(ctx) (bool, error)
func (s *Service) TokenConfigured() bool  // SetupToken != ""
func (s *Service) Setup(ctx, setupToken, username string, c Credentials) (store.User, error) // username sai ValidUsername → ErrInvalid; SetupToken rỗng hoặc sai → ErrBadSetupToken; đã có user → ErrSetupDone; dùng Store.CreateFirstAdmin
func (s *Service) LoginStart(ctx, clientFirst, ip string) (serverFirst, scramID string, att Attempt, err error)        // att luôn điền khi parse được username, kể cả khi ErrLocked
func (s *Service) LoginFinish(ctx, scramID, clientFinal, ip, userAgent string) (serverFinal, sessionID string, u store.User, att Attempt, err error) // att điền cả khi ErrBadCredentials
func (s *Service) ReauthStart(ctx, p Principal, clientFirst, ip string) (serverFirst, scramID string, err error) // username trong clientFirst phải = p.Username; bị khóa (cùng bộ đếm login theo username|ip) → ErrLocked
func (s *Service) ReauthFinish(ctx, p Principal, scramID, clientFinal, ip string) (serverFinal, reauthToken string, err error) // sai → đếm vào cùng bộ đếm login, ErrBadCredentials
func (s *Service) Authenticate(ctx, sessionID string) (Principal, error) // hash → GetSession; quá idle/expires hoặc user disabled → ErrUnauthenticated (và xóa row); Touch nếu last_seen cũ > 1 phút
func (s *Service) Logout(ctx, p Principal) error
func (s *Service) ChangePassword(ctx, p Principal, reauthToken string, c Credentials) error // xóa mọi session khác
func (s *Service) CreateInvite(ctx, p Principal, username, role string) (token string, err error) // chỉ admin; username sai ValidUsername → ErrInvalid; đã tồn tại → store.ErrConflict
func (s *Service) CreateResetInvite(ctx, createdBy *int64, userID int64) (token string, err error) // dùng cho admin + recover
func (s *Service) InviteInfo(ctx, token string) (username, purpose string, err error)               // không hợp lệ/hết hạn/đã dùng → ErrInviteInvalid
func (s *Service) AcceptInvite(ctx, token string, c Credentials) (store.User, error)                // new_user → kiểm ValidUsername (ErrInvalid) → CreateUser; reset → UpdateUserCredentials + xóa session; MarkInviteUsed
func (s *Service) ListUsers(ctx, p Principal) ([]store.User, error)              // chỉ admin
func (s *Service) SetRole(ctx, p Principal, userID int64, role string) error     // chỉ admin; store.ErrLastAdmin → ErrLastAdmin
func (s *Service) SetDisabled(ctx, p Principal, userID int64, disabled bool) error // chỉ admin; store.ErrLastAdmin → ErrLastAdmin; khóa → DeleteUserSessions
func (s *Service) Promote(ctx, username string) error                            // không cần principal (recover)
func (s *Service) ListSessions(ctx, p Principal, all bool) ([]store.Session, error) // all chỉ admin
func (s *Service) RevokeSession(ctx, p Principal, idHash []byte) error              // của mình hoặc admin; không thấy → store.ErrNotFound
func (s *Service) PurgeExpired(ctx) (int, error)
func SessionHash(sessionID string) []byte
```
- Hằng số đúng Global Constraints (iterations, TTL, rate limit…), khai báo `const` có tên; `now` chỉnh được trong test.
- User không tồn tại / bị khóa ở `LoginStart`: salt giả `HMAC-SHA256(Secret, "fridge-fake-salt:"+username)[:16]`, iterations 600000, vẫn trả `serverFirst`; ở `LoginFinish` chạy `Finish` với khóa ngẫu nhiên rồi trả `ErrBadCredentials` (cùng lỗi như sai mật khẩu). Rate limit đếm lần sai (login **và** reauth) theo `username+"|"+ip`; khi bị khóa → `ErrLocked` ngay ở `LoginStart`/`ReauthStart`.
- Last-admin: không tự đếm rồi ghi (race) — dựa vào `Store.SetUserRole/SetUserDisabled` đã guard trong transaction (Task 1).
- Token (session, invite, reauth): 32 byte `crypto/rand`, base64url không padding; chỉ hash vào DB/RAM.

- [ ] **Step 1: Viết test** (store thật qua `storetest`, client dùng `scram.NewClient`, iterations 600000 thật — chấp nhận chậm ~1s/test; helper `creds(password)` sinh Credentials; username `alice`, `bob`, `carol`)

```go
func TestSetup(t *testing.T)               // SetupToken "" → ErrBadSetupToken; sai token → ErrBadSetupToken; đúng → admin "alice"; lần 2 → ErrSetupDone; SetupNeeded false; TokenConfigured() đúng theo SetupToken
func TestUsernameRules(t *testing.T)       // Setup với "alice_x", "Al", "ab", "a-b" → ErrInvalid; CreateInvite "bob_1" → ErrInvalid; "bob" OK
func TestLoginSuccessAndSession(t *testing.T) // start/finish → client.Verify(serverFinal)==nil; Authenticate(sessionID) trả Principal đúng role
func TestSessionStoredHashed(t *testing.T) // row sessions.id_hash == sha256(sessionID), không có cột nào chứa sessionID thô
func TestLoginWrongPasswordGeneric(t *testing.T) // sai mật khẩu và user không tồn tại đều ErrBadCredentials; serverFirst của user không tồn tại có dạng hợp lệ (s=, i=600000) và ổn định giữa 2 lần
func TestLoginAttemptInfo(t *testing.T)    // sai mật khẩu "alice" → att.Username "alice", att.UserID != nil; "nobody" → att.Username "nobody", UserID nil; bị khóa → ErrLocked kèm att.Username
func TestLoginRateLimit(t *testing.T)      // 5 lần sai cùng (user, ip) → LoginStart lần 6 ErrLocked; IP khác vẫn thử được; Now +5m → mở khóa
func TestReauthSharesLimiter(t *testing.T) // 5 lần login sai ("alice", ip1) → ReauthStart của alice từ ip1 → ErrLocked; 5 lần ReauthFinish sai → LoginStart ("alice", ip1) → ErrLocked
func TestScramStateExpires(t *testing.T)   // Now +61s giữa start và finish → ErrBadCredentials
func TestSessionExpiry(t *testing.T)       // Now +12h01m không hoạt động → ErrUnauthenticated; hoạt động mỗi giờ nhưng quá 7 ngày → ErrUnauthenticated
func TestLogoutAndChangePassword(t *testing.T) // Logout → ErrUnauthenticated; ChangePassword không reauth token / token hết hạn → ErrForbidden; có token → mật khẩu mới đăng nhập được, session khác bị xóa, session hiện tại còn
func TestInviteFlow(t *testing.T)          // user thường CreateInvite → ErrForbidden; admin tạo "bob" → InviteInfo đúng; Accept → đăng nhập được; Accept lần 2 → ErrInviteInvalid; Now +49h → ErrInviteInvalid
func TestResetInvite(t *testing.T)         // reset → mật khẩu cũ hỏng, session cũ bị xóa
func TestLastAdminGuard(t *testing.T)      // 1 admin: SetRole(user)/SetDisabled(true) chính nó → ErrLastAdmin; thêm admin thứ 2 rồi khóa admin 1 → OK và session của admin 1 → ErrUnauthenticated; Promote("bob") → role admin
func TestRevokeSession(t *testing.T)       // user không revoke được session người khác (ErrNotFound); admin được
```
- [ ] **Step 2:** `bash scripts/test-db.sh && go test ./internal/auth/` → FAIL. **Step 3:** implement. **Step 4:** PASS + `go test -race ./internal/auth/`.
- [ ] **Step 5: Commit** `feat(auth): setup, SCRAM login, stateful sessions, invites, users`.

---

### Task 5: App — helper quyền, members, duyệt file restore, destination đã xóa, Backup now

**Files:**
- Create: `internal/app/access.go`, `internal/app/members.go`, tests
- Modify: `internal/app/databases.go` (OwnerID, `defaultSchedule`), `internal/app/backup.go` (`StartBackup` thêm `startedBy`, quy tắc fallback), `internal/app/destinations.go` (`objects()` chặn destination đã xóa), `internal/app/restore.go` (OwnerID), `internal/app/scheduler.go` (call site), `internal/app/fakes_test.go`, call site trong `internal/api/api.go` (chỉ cho biên dịch)

**Interfaces:**
- Consumes: store Task 2, `auth.Principal` (Task 4).
- Produces:

```go
// CreateRequest thêm OwnerID int64 — BẮT BUỘC > 0, ngược lại ErrInvalid (không còn trường hợp "không owner").
// RestoreRequest thêm OwnerID int64 (bắt buộc > 0) → owner node mới & restores.started_by_id.
// ĐỔI chữ ký: func (a *App) StartBackup(ctx, dbID, destID int64, scheduleID *int64, trigger string, startedBy *int64) (store.Backup, error)
//   startedBy nil = hệ thống (scheduler); ghi backups.started_by_id.
//   destID == 0 và DB không có lịch: chọn destination owner DB DÙNG ĐƯỢC (owner sở hữu hoặc shared, chưa xóa, id nhỏ nhất);
//   không có → ErrInvalid "no usable destination". KHÔNG BAO GIỜ lấy ListDestinations()[0].
func (a *App) VisibleDatabases(ctx, p auth.Principal) ([]store.Database, error) // admin: ListDatabases; user: ListDatabasesVisible
func (a *App) CanAccessDB(ctx, p auth.Principal, d store.Database) (bool, error) // admin || owner || member
func IsDBOwner(p auth.Principal, d store.Database) bool                          // admin || owner
func (a *App) VisibleDestinations(ctx, p auth.Principal) ([]store.Destination, error)
func CanManageDestination(p auth.Principal, d store.Destination) bool // admin || owner
func CanUseDestination(p auth.Principal, d store.Destination) bool    // (manage || shared) && DeletedAt == nil
func (a *App) SetDestinationShared(ctx, id int64, shared bool) error
func (a *App) AddMember(ctx, d store.Database, username string, addedBy int64) error // username không có, owner tự thêm mình, đã là member → cùng một lỗi ErrInvalid "không thêm được" (không lộ username có tồn tại)
func (a *App) RemoveMember(ctx, d store.Database, userID int64) error
func (a *App) ListMembers(ctx, d store.Database) ([]store.Member, error)
type Restorable struct { Key string; Size int64; LastModified time.Time; DatabaseName string /* rỗng nếu file lạ */ }
func (a *App) ListRestorable(ctx, p auth.Principal, destID int64, prefix, q string) ([]Restorable, bool, error)
	// dest không thấy → store.ErrNotFound; CanManage → liệt kê bucket (ListObjects); shared không manage → ListOKBackupsOnDestination(visibleTo=&p.UserID) lọc theo prefix/q
func (a *App) CanRestoreKey(ctx, p auth.Principal, destID int64, key string) (bool, error)
	// CanManage(dest) || (GetBackupByKey(dest,key) thuộc DB mà CanAccessDB)
```
- **Destination đã xóa mềm** (`DeletedAt != nil`): kiểm **một chỗ** trong `a.objects()` → `ErrInvalid` "destination deleted"; vì mọi đường (`TestDestination`, `ListRestorable`, `BackupDownloadURL`, `StartBackup`, `StartRestore`, và `StartDumpCheck` ở Task 14) đều đi qua `a.objects()`.
- `defaultSchedule` chỉ chọn destination **owner của DB dùng được** (owner sở hữu hoặc shared, chưa xóa, id nhỏ nhất); không có → không tạo lịch.
- Scheduler gọi `StartBackup(…, "schedule", nil)`.
- `internal/api`: chỉ sửa call site cho `go build ./...` qua; test API tạo DB/restore sẽ fail cho tới Task 9 (chưa có user để làm owner) — **không** sửa test API ở task này.

- [ ] **Step 1: Viết test** (fake runtime/objects sẵn có; users `alice`, `bob`, admin `root1` tạo bằng store)

```go
func TestOwnershipOnCreate(t *testing.T)   // CreateDatabase OwnerID=alice → d.OwnerID==alice; OwnerID=0 → ErrInvalid; restore OwnerID=bob → node mới owner bob, restores.started_by_id==bob; RestoreRequest OwnerID=0 → ErrInvalid
func TestAccessHelpers(t *testing.T)       // bảng: admin/owner/member/khác × CanAccessDB, IsDBOwner; CanManage/CanUse với dest riêng/shared/đã xóa
func TestMembersApp(t *testing.T)          // AddMember "nobody", AddMember owner, AddMember trùng → cùng ErrInvalid, cùng thông điệp "không thêm được"; thêm bob → bob thấy DB qua VisibleDatabases; Remove → mất
func TestListRestorableShared(t *testing.T) // dS shared (admin); backup alice_one (alice) + bob_two (bob) trên dS → alice: chỉ key alice_one, có DatabaseName; admin: liệt kê bucket (fakeObjects có thêm "upload/x.sql.gz")
func TestListRestorablePrivateHidden(t *testing.T) // dest riêng của bob → alice nhận store.ErrNotFound
func TestCanRestoreKey(t *testing.T)       // alice với key backup bob_two trên dS → false; key alice_one → true; bob trên dest của mình với file lạ → true
func TestDefaultScheduleUsesOwnersDestination(t *testing.T) // dest riêng của admin không được dùng cho DB của alice; có dS shared → dùng dS
func TestBackupNowFallbackOwnerDestination(t *testing.T) // DB alice không lịch; chỉ có dest riêng của admin → StartBackup(destID 0) → ErrInvalid, không có backup row; thêm dS shared → dùng dS; startedBy=&alice → backup.StartedByID == alice
func TestDeletedDestinationBlocked(t *testing.T) // backup ok trên dest rồi xóa mềm dest → BackupDownloadURL / StartRestore theo key đó / TestDestination → ErrInvalid chứa "destination deleted"; lịch sử backup vẫn liệt kê được
```
- [ ] **Step 2:** FAIL. **Step 3:** implement; cập nhật call site/tests cũ trong `internal/app` (`CreateRequest`/`RestoreRequest` thêm OwnerID, `StartBackup` thêm tham số). **Step 4:** `go build ./... && go test ./internal/app/ && go test -race ./internal/app/` PASS.
- [ ] **Step 5: Commit** `feat(app): resource ownership, members and access helpers`.

---

### Task 6: App — tiền tố tên DB/user, credentials tự đặt, domain đã đăng ký

**Files:**
- Create: `internal/app/names.go` + `names_test.go`
- Modify: `internal/app/databases.go`, `internal/app/restore.go`, `internal/engine/postgres.go` (`ConnString`), `internal/app/fakes_test.go`

**Interfaces:**
- Consumes: Task 5 (`CreateRequest.OwnerID`), store domains (Task 2).
- Produces:

```go
// CreateRequest thêm DBUser, DBPassword string. RestoreRequest.New (CreateRequest) mang DBUser/DBPassword.
// Tên DB BẮT BUỘC tiền tố: CreateRequest.Name là HẬU TỐ ^[a-z0-9][a-z0-9-]{0,29}$ (không "_");
//   tên thật = "<owner username>_" + Name. Client gửi kèm sẵn đúng tiền tố "<owner>_" của chính owner → bỏ tiền tố rồi kiểm;
//   phần còn lại chứa "_" (vd "bob_x") → ErrInvalid. Tên thật dùng cho hiển thị, {db}, và database bên trong engine (POSTGRES_DB).
//   Trùng → ErrConflict (chỉ có thể trùng với DB của chính owner).
// DBUser là HẬU TỐ: tên thật = "<owner username>_" + DBUser; DBUser ^[a-z0-9_]{1,15}$ (kèm sẵn đúng tiền tố của owner → bỏ rồi kiểm);
//   rỗng → hậu tố = hậu tố tên DB ("-"→"_", cắt 15 ký tự). Tên thật trùng user ứng dụng của một DB đang hoạt động KHÁC của cùng owner → ErrConflict (không bao giờ so với owner khác).
// DBPassword: 12–128 ký tự ASCII in được (0x21–0x7E), sai → ErrInvalid; rỗng → rand.Text().
// CreateRequest.Domain phải là domain đã đăng ký của owner (store.GetDomainByName + OwnerID khớp) → không thì ErrInvalid "domain not registered"; d.DomainID set.
func dbName(owner, suffix string) (string, error)   // hàm thuần, dùng chung create/restore
func dbUser(owner, suffix, dbSuffix string) (string, error)
func validDBPassword(pw string) error
// engine.postgres.ConnString: dùng url.UserPassword(user, password) thay QueryEscape.
```
- Ở task này node vẫn một user (user ứng dụng = `POSTGRES_USER`); Task 7 đổi sang hai user.

- [ ] **Step 1: Viết test** (owner `alice`, `bob`; domain `alice.lan` của alice, `bob.lan` của bob đăng ký sẵn)

```go
func TestDBNamePrefix(t *testing.T)        // owner alice: Name "shop" → d.Name "alice_shop", POSTGRES_DB=alice_shop; Name "alice_shop" → "alice_shop"; Name "bob_x" → ErrInvalid (hậu tố tên DB không có "_"); "Shop", "-x", hậu tố 31 ký tự → ErrInvalid; bob tạo "shop" khi alice có → "bob_shop" OK; alice dùng domain chưa đăng ký hoặc "bob.lan" → ErrInvalid
func TestDBUserPrefix(t *testing.T)        // owner alice: DBUser "shop" → user "alice_shop"; "alice_shop" → "alice_shop" (không nhân đôi); "bob_x" → "alice_bob_x" (hậu tố user cho phép "_", tiền tố owner khác chỉ là một phần hậu tố); rỗng với DB "my-db" → "alice_my_db"; hậu tố 16 ký tự hoặc có "-" → ErrInvalid; bob tạo DBUser "shop" khi alice đã có "alice_shop" → OK ("bob_shop"); alice tạo DB thứ hai với DBUser "shop" khi "alice_shop" đang hoạt động → ErrConflict
func TestCustomDBCredentials(t *testing.T) // owner alice, DBUser "shop_rw", DBPassword "S3cure-pass.~x" → Spec env POSTGRES_USER=alice_shop_rw, POSTGRES_PASSWORD=mật khẩu đó; ConnString giải mã đúng qua url.Parse (User.Username()=="alice_shop_rw", User.Password()==mật khẩu); bảng lỗi mật khẩu: 11 ký tự, có dấu cách, có ký tự non-ASCII → ErrInvalid; để trống mật khẩu → 26 ký tự ngẫu nhiên
func TestRestoreNamePrefix(t *testing.T)   // restore vào node mới với New.Name "copy", OwnerID=alice → node "alice_copy"
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/app/ ./internal/engine/` PASS.
- [ ] **Step 5: Commit** `feat(app): owner-prefixed DB and user names, custom credentials, registered domains`.

---

### Task 7: App/engine — hai user trên mỗi node (superuser riêng + user ứng dụng)

**Files:**
- Create: `internal/app/superuser.go` + test
- Modify: `internal/engine/engine.go`, `internal/engine/postgres.go` (+ test), `internal/app/databases.go` (`provision`, đặt tên theo node_key), `internal/app/backup.go`, `internal/app/restore.go`, `internal/app/fakes_test.go`

**Interfaces:**
- Consumes: Task 6 (tên thật, DBUser/DBPassword), store `AdminUser/AdminPasswordEnc/NodeKey` (Task 2).
- Produces:

```go
// engine
type Params struct { Name, User, Password, Version, AdminUser, AdminPassword string } // AdminUser rỗng (DB kiểu cũ) → app truyền AdminUser = User
type Engine interface {
	Spec(p Params) Spec                      // POSTGRES_USER=AdminUser, POSTGRES_PASSWORD=AdminPassword, POSTGRES_DB=Name; healthcheck `pg_isready -h 127.0.0.1 -U <AdminUser> -d <Name>`
	DumpCmd(p Params) []string               // -U AdminUser
	DumpExt() string
	RestoreCmd(p Params, key string) (cmd []string, gunzip, setRole bool, err error)
	    // .dump → pg_restore -U AdminUser -d Name --no-owner --no-acl --exit-on-error --role=<User>, setRole=false
	    // .sql / .sql.gz → psql -U AdminUser …, setRole=true (app ghép "SET ROLE \"<User>\";\n" vào đầu stdin)
	ConnString(p Params, domain string) string // user ứng dụng (User/Password), url.UserPassword
	AppUserSQL(user, password, db string) string
	    // CREATE ROLE "<user>" LOGIN PASSWORD '<escaped>' NOSUPERUSER NOCREATEDB NOCREATEROLE; ALTER DATABASE "<db>" OWNER TO "<user>"; ALTER SCHEMA public OWNER TO "<user>";
	    // định danh qua quote ident (" → ""), mật khẩu escape ' → ''
	SetPasswordSQL(user, password string) string // ALTER ROLE "<user>" PASSWORD '<escaped>';
	AdminCmd(p Params) []string                  // psql -U AdminUser -d Name -v ON_ERROR_STOP=1 -q (SQL qua stdin)
}
// app
//   provision(): NodeKey = hex(sha256(ownerUsername + "_" + name + "_" + strconv.FormatInt(now.UnixNano(), 10)))[:16];
//     container + volume = "fridge-<engine>-<NodeKey>"; superuser = "su_" + NodeKey, mật khẩu rand.Text(); lưu NodeKey, AdminUser, AdminPasswordEnc.
//     Run container → WaitHealthy → Exec AdminCmd với stdin = AppUserSQL(app user, app password, Name) → lỗi thì rollback như cũ.
//     Mật khẩu không bao giờ nằm trong argv.
//   DB kiểu cũ (NodeKey/AdminUser rỗng): giữ tên container/volume cũ, mọi lệnh dùng db_user như trước.
//   Restore vào node mới sinh NodeKey mới.
func (a *App) RevealSuperuser(ctx, d store.Database) (user, password string, err error) // DB kiểu cũ → user/mật khẩu của db_user
func (a *App) RotateSuperuser(ctx, d store.Database) error // sinh mật khẩu mới → Exec AdminCmd stdin SetPasswordSQL → exit 0 mới lưu; node không running → ErrInvalid; DB kiểu cũ → ErrInvalid
```

- [ ] **Step 1: Viết test** (owner `alice`, `bob`)

```go
func TestPostgresTwoUserSpec(t *testing.T) // engine: Spec env POSTGRES_USER=su_…; RestoreCmd(".dump") chứa "--role=alice_shop", setRole false; RestoreCmd(".sql.gz") gunzip true, setRole true; AppUserSQL quote ident `"a""b"` và mật khẩu `it''s`
func TestTwoUsersPerNode(t *testing.T)     // alice tạo "shop" và "blog" → RunSpec Name = "fridge-postgres-<16 hex>" khác nhau, POSTGRES_USER = "su_<cùng 16 hex của node đó>", mật khẩu khác nhau; Exec sau healthy là psql -U su_<key> với stdin chứa `CREATE ROLE "alice_shop" LOGIN`, `NOSUPERUSER`, `OWNER TO "alice_shop"`, mật khẩu không nằm trong argv; DumpCmd dùng -U su_<key>
func TestCustomCredentialsTwoUser(t *testing.T) // DBUser "shop_rw", DBPassword "S3cure-pass.~x" → stdin SQL tạo role "alice_shop_rw" với mật khẩu đó; POSTGRES_USER là su_<key>, không phải alice_shop_rw
func TestRotateSuperuser(t *testing.T)     // rotate → Exec ALTER ROLE "su_<key>" (stdin) rồi mật khẩu lưu đổi; execFn lỗi → mật khẩu lưu giữ nguyên, trả lỗi; DB kiểu cũ → ErrInvalid
func TestRevealSuperuser(t *testing.T)     // trả "su_<key>" + mật khẩu giải mã; DB kiểu cũ → db_user + mật khẩu của nó
func TestRestoreUsesAppRole(t *testing.T)  // restore .dump → cmd chứa "--role=<app user>"; restore .sql.gz → stdin bắt đầu bằng `SET ROLE "<app user>";`
func TestLegacyDBUsesSingleUser(t *testing.T) // DB có AdminUser "" → DumpCmd -U <db_user>, tên container giữ nguyên
```
- [ ] **Step 2:** FAIL. **Step 3:** implement; sửa test cũ của Task 6 (`TestCustomDBCredentials` giờ khẳng định qua stdin SQL — giữ assertion ConnString). **Step 4:** `go test ./internal/engine/ ./internal/app/ && go test -race ./internal/app/` PASS.
- [ ] **Step 5: Commit** `feat(app): per-node superuser and non-superuser app user`.

---

### Task 8: Cô lập mạng — network riêng cho từng node DB

**Files:**
- Create: `internal/container/network.go`, `internal/container/network_integration_test.go` (`//go:build integration`), `internal/app/network.go` + `network_test.go`
- Modify: `internal/container/docker.go` (`RunSpec.Network`), `internal/app/app.go` (`Runtime` thêm method network; `App` thêm `ControlNetwork`, `TraefikContainer`), `internal/app/databases.go` (`provision`, `removeNode`), `internal/app/fakes_test.go`, `cmd/fridge/main.go`

**Interfaces:**
- Consumes: Task 7 (NodeKey, tên container `fridge-<engine>-<NodeKey>`).
- Produces:

```go
// container
type RunSpec struct { /* cũ */; Network string } // "" = c.network (fridge-net); DB node: NodeNetwork(d). Alias = Name trên network đó.
func (c *Client) CreateNetwork(ctx, name string) error                 // driver bridge, Internal: true, label fridge.node=1; đã có → nil
func (c *Client) RemoveNetwork(ctx, name string) error                 // không có → nil
func (c *Client) ConnectNetwork(ctx, netName, containerName string, aliases []string) error // đã nối → nil
func (c *Client) DisconnectNetwork(ctx, netName, containerName string) error                // chưa nối/không có → nil
func (c *Client) ContainerNetworks(ctx, containerName string) ([]string, error)
func (c *Client) Subnets(ctx, names ...string) ([]netip.Prefix, error) // IPAM subnet của các network tên đúng bằng names (dùng cho proxy tin cậy, Task 9)
func (c *Client) FridgeSubnets(ctx) ([]netip.Prefix, error)            // c.network + mọi network tên bắt đầu "fridge-node-" (dùng cho guard SSRF, Task 12)
// app.Runtime thêm: CreateNetwork, RemoveNetwork, ConnectNetwork, DisconnectNetwork, ContainerNetworks, FridgeSubnets (chữ ký như trên)
// App thêm: ControlNetwork string (= FRIDGE_NETWORK, mặc định "fridge-net"); TraefikContainer string (= FRIDGE_TRAEFIK_CONTAINER, mặc định "fridge-traefik")
func NodeNetwork(d store.Database) string // "fridge-node-" + NodeKey; DB kiểu cũ (NodeKey rỗng) → "fridge-node-legacy-<id>"
func (a *App) ReconcileNetworks(ctx) error
	// mỗi DB status running|stopped: CreateNetwork(net) → ConnectNetwork(net, TraefikContainer, nil);
	// container DB chưa nối net → ConnectNetwork(net, container, []string{container}); đang nối ControlNetwork → DisconnectNetwork(ControlNetwork, container).
	// Lỗi từng DB → log + alert, đi tiếp; file route không đổi (alias = tên container).
```
- `provision()` theo thứ tự: `CreateNetwork(net)` → `ConnectNetwork(net, TraefikContainer, nil)` → `Run(RunSpec{…, Network: net})` → healthy → SQL user ứng dụng → cert/route. Lỗi ở bất kỳ bước nào → rollback: `Remove` container/volume (như cũ) → `DisconnectNetwork(net, TraefikContainer)` → `RemoveNetwork(net)`.
- `removeNode()`: `Remove` container/volume → `DisconnectNetwork(net, TraefikContainer)` → `RemoveNetwork(net)` (lỗi chỉ log).
- `main.go`: đọc `FRIDGE_TRAEFIK_CONTAINER` (mặc định `fridge-traefik`), gán `ControlNetwork`/`TraefikContainer`; gọi `a.ReconcileNetworks(ctx)` sau `RecoverInterrupted`, trước khi phục vụ (lỗi → log, không dừng server).
- Fridge **không** gắn vào network node (healthcheck/backup/restore đều qua Docker exec).

- [ ] **Step 1: Viết test**

```go
// unit (fakeRuntime ghi lại mọi lệnh network theo thứ tự)
func TestProvisionCreatesNodeNetwork(t *testing.T)  // CreateDatabase → CreateNetwork("fridge-node-<key>"), ConnectNetwork(net, "fridge-traefik"), Run với Network==net; route Backend vẫn "<tên container>:5432"; không có lệnh nào nối container DB vào "fridge-net"
func TestProvisionRollbackRemovesNetwork(t *testing.T) // WaitHealthy lỗi → Remove container, DisconnectNetwork(net,"fridge-traefik"), RemoveNetwork(net) theo đúng thứ tự; ConnectNetwork Traefik lỗi → RemoveNetwork(net), không Run
func TestRemoveNodeRemovesNetwork(t *testing.T)     // removeNode → Disconnect Traefik rồi RemoveNetwork
func TestReconcileNetworksLegacy(t *testing.T)      // DB kiểu cũ id 7 có container trên "fridge-net" → CreateNetwork("fridge-node-legacy-7"), Connect Traefik, Connect container với alias = tên container, Disconnect "fridge-net"
func TestReconcileNetworksIdempotent(t *testing.T)  // chạy 2 lần trên DB mới → lần 2 không Disconnect nào, Connect chỉ là no-op (fake trả nil khi đã nối)
// integration (//go:build integration, internal/container), Docker thật, network điều khiển tạm "fridge-it-net":
func TestNodeNetworkIsolation(t *testing.T)
	// pgA trên "fridge-it-node-a" (Internal), pgB trên "fridge-it-node-b"; "fridge-it-meta" (postgres) và "fridge-it-proxy" (postgres, đóng vai Traefik) trên fridge-it-net;
	// ConnectNetwork(node-a, proxy), ConnectNetwork(node-b, proxy)
	// Exec trong pgA: `pg_isready -h <pgB> -t 3` exit ≠ 0; `pg_isready -h fridge-it-meta -t 3` exit ≠ 0
	// Exec trong proxy: `pg_isready -h <pgA> -t 5` exit 0
	// Subnets(ctx, "fridge-it-net") trả 1 prefix chứa IP của proxy; FridgeSubnets không lỗi
	// dọn mọi container/network fridge-it-*
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/app/ ./internal/container/ && go test -race ./internal/app/` + `go test -tags integration ./internal/container/ -run NodeNetwork -v` PASS.
- [ ] **Step 5: Commit** `feat: isolate each DB node on its own internal Docker network`.

---

### Task 9: API — xác thực, route auth/user/session, header bảo mật, job cố định

**Files:**
- Create: `internal/api/middleware.go`, `internal/api/auth_routes.go`, tests
- Modify: `internal/api/api.go` (chữ ký `New`, bỏ token tĩnh, `/api/ca` công khai), `internal/app/scheduler.go` (`AddFixed`) + test, `cmd/fridge/main.go`

**Interfaces:**
- Consumes: `auth.Service` (Task 4), `container.Client.Subnets` (Task 8).
- Produces:

```go
type Options struct {
	Web          http.Handler       // nil → 404 cho đường dẫn ngoài /api, /healthz, /readyz
	TrustedProxy func(ip net.IP) bool // nil → chỉ loopback
}
func New(a *app.App, au *auth.Service, o Options) http.Handler
func principal(r *http.Request) (auth.Principal, bool) // lấy từ context
func clientIP(r *http.Request, trusted func(net.IP) bool) string // RemoteAddr; nếu peer tin cậy → phần tử **cuối** của X-Forwarded-For
func TrustCIDRs(prefixes []netip.Prefix) func(net.IP) bool // tin peer thuộc một prefix hoặc loopback
func ParseCIDRs(s string) ([]netip.Prefix, error)          // "10.0.0.0/8, 172.20.0.0/16" → prefixes; rỗng → nil
// app
func (s *Scheduler) AddFixed(spec string, fn func()) error // job cố định, không bị Reload gỡ
// Reload: chỉ gỡ các entry ID của lịch backup đã đăng ký lần trước (Scheduler giữ []cron.EntryID), không bao giờ gỡ job AddFixed.
```
- Route **công khai**: `GET /healthz`, `GET /readyz`, `GET /api/ca`, `GET /api/setup/status` → `{"needed":bool,"token_configured":bool}`, `POST /api/setup` `{setup_token, username, credentials}`, `POST /api/auth/scram/start` `{client_first}` → `{server_first, scram_id}`, `POST /api/auth/scram/finish` `{scram_id, client_final, want_token?}` → `{server_final, user:{id,username,role}, session_id?}` + Set-Cookie, `POST /api/invites/lookup` `{token}` → `{username, purpose}`, `POST /api/invites/accept` `{token, credentials}`. **Không** có route nào nhận invite token trong đường dẫn.
- Route **cần đăng nhập**: `POST /api/auth/logout`, `GET /api/me`, `POST /api/me/reauth/start|finish` (finish → `{server_final, reauth_token}`), `POST /api/me/password` `{reauth_token, credentials}`, `GET /api/me/sessions`, `DELETE /api/sessions/{id}` (`id` = hex của id_hash). **Admin**: `GET /api/users`, `POST /api/invites` `{username, role}` → `{link}`, `POST /api/users/{id}/reset` → `{link}`, `PATCH /api/users/{id}` `{role?, disabled?}`, `GET /api/sessions`.
- Session JSON (cả `/api/me/sessions` và `/api/sessions`): `{id, user_id, username, created_at, last_seen_at, ip, user_agent, current}` — `current: true` khi `id_hash` = `SessionHash` của principal đang gọi.
- `credentials` JSON: `{salt, iterations, stored_key, server_key}` (base64 chuẩn).
- `session_id` chỉ có trong body khi request gửi `"want_token": true` (CLI). Cookie luôn được set.
- Link mời: `https://<r.Host>/#/invite/<token>` (token sau `#`, không gửi lên server khi mở trang).
- Middleware: principal từ cookie `fridge_session` hoặc `Authorization: Bearer <session>`; request dùng **cookie** + method không phải GET/HEAD/OPTIONS phải có `Origin` khớp `https://`+`r.Host` (hoặc `Sec-Fetch-Site: same-origin`) → không thì 403. Chưa setup + không principal → 401 `setup required`. Header mọi response: `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`.
- Map lỗi: `ErrBadCredentials/ErrUnauthenticated` 401, `ErrBadSetupToken/ErrForbidden` 403, `ErrSetupDone/ErrLastAdmin/ErrConflict` 409, `ErrLocked` 429, `ErrInvalid/ErrInviteInvalid` 400.
- Route cũ giữ nguyên handler nhưng đặt sau middleware (áp quyền ở Task 10). Tạm thời: user đã đăng nhập dùng được mọi route cũ; `createDB` và `startRestore` gán `OwnerID = p.UserID` ngay (Task 5 bắt buộc OwnerID); Backup now truyền `startedBy = &p.UserID`.
- `main.go`: `auth.New(store, secretKeyBytes, env FRIDGE_SETUP_TOKEN)`; chưa có user và `FRIDGE_SETUP_TOKEN` rỗng → `log.Printf("WARNING: no users yet and FRIDGE_SETUP_TOKEN is not set — setup is impossible until it is")`. Proxy tin cậy: `FRIDGE_TRUSTED_PROXY` khác rỗng → `ParseCIDRs` (lỗi → thoát với lỗi); ngược lại `rt.Subnets(ctx, network)` (subnet `fridge-net`); lỗi → log, chỉ tin loopback. Bỏ yêu cầu `FRIDGE_TOKEN`. `a.Sched.AddFixed("@hourly", func(){ au.PurgeExpired(ctx) })` (lỗi → log). Sửa test API cũ dùng đăng nhập thay token.

- [ ] **Step 1: Viết test** (httptest + store thật + `scram.NewClient`; helper `loginAs(t, srv, user, pw) (cookie, sessionID)`; user `alice`, `bob`)

```go
func TestSetupRequired(t *testing.T)       // chưa setup: GET /api/databases → 401 {"error":"setup required"}; /api/setup/status → needed true, token_configured đúng theo SetupToken; /api/ca → 200 PEM
func TestSetupAndLoginWeb(t *testing.T)    // setup → scram start/finish → Set-Cookie fridge_session có HttpOnly, Secure, SameSite=Strict; GET /api/me bằng cookie → username
func TestWebLoginNoTokenInBody(t *testing.T) // finish không want_token → body không có "session_id"; có want_token → có và Bearer dùng được
func TestInviteTokenNotInPath(t *testing.T) // admin POST /api/invites → link; POST /api/invites/lookup {token} → username/purpose; GET /api/invites/<token> → 404 (route không tồn tại); POST /api/invites/accept {token, credentials} → đăng nhập được
func TestCSRF(t *testing.T)                // POST bằng cookie, Origin "https://evil" → 403; Origin đúng → qua; Bearer không Origin → qua
func TestClientIPTrustsOnlyProxy(t *testing.T) // TrustCIDRs([172.30.0.0/16]): peer 10.9.9.9 + XFF "1.2.3.4" → "10.9.9.9"; peer 172.30.0.5 + XFF "1.1.1.1, 5.6.7.8" → "5.6.7.8"; peer 127.0.0.1 tin; ParseCIDRs("bad") → lỗi
func TestAdminOnlyRoutes(t *testing.T)     // user thường GET /api/users → 403; admin POST /api/invites → link chứa "/#/invite/"
func TestSessionListCurrent(t *testing.T)  // alice có 2 phiên; GET /api/me/sessions bằng phiên 1 → đúng 1 phần tử current:true và là phiên 1
func TestSecurityHeaders(t *testing.T)     // mọi response có CSP "default-src 'self'"
func TestLogoutInvalidatesBearer(t *testing.T)
// internal/app (package app)
func TestFixedJobsSurviveReload(t *testing.T) // AddFixed("@hourly", fn) + 1 lịch enabled → Reload 2 lần → len(s.c.Entries()) == 2 và entry của job cố định vẫn còn (cùng EntryID); xóa lịch → Reload → còn đúng 1 entry (job cố định)
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/api/ ./internal/app/ ./cmd/fridge/` PASS.
- [ ] **Step 5: Commit** `feat(api): session auth, SCRAM routes, users and invites`.

---

### Task 10: API — áp quyền trên mọi route tài nguyên

**Files:** Modify `internal/api/api.go`, `internal/store/jobs.go` (`LastBackups`); Create `internal/api/admin_routes.go` (members, share, domains, superuser), `internal/api/authz_test.go`

**Interfaces:**
- Consumes: helper Task 5, tên/credentials Task 6, superuser Task 7, principal Task 9.
- Produces:
  - `listDBs` → `VisibleDatabases`; `dbOf` → 404 nếu `!CanAccessDB`; `createDB` → `OwnerID = p.UserID`, body thêm `db_user`, `db_password` (tùy chọn); `POST /api/restores` `new` thêm `db_user`, `db_password`.
  - `dbJSON` thêm `owner`; `connection_string` trong `GET /api/databases` và `GET /api/databases/{db}` **luôn che mật khẩu** (`postgresql://alice_shop:****@…`). `db_password` không bao giờ xuất hiện trong response.
  - `GET /api/databases/{db}/connection` → `{connection_string}` **đầy đủ** (quyền như đọc DB); `?role=superuser` → chuỗi của superuser, chỉ owner/admin (người quản lý → 403). Ghi audit `db.reveal_password` thêm ở Task 11.
  - List `GET /api/databases`: mỗi DB thêm `last_backup: {status, finished_at}` (null nếu chưa có) — `func (s *Store) LastBackups(ctx, dbIDs []int64) (map[int64]store.Backup, error)` (backup mới nhất theo `started_at` mỗi DB).
  - Domains: `GET /api/domains` (user: của mình; admin: tất cả + owner), `POST /api/domains` `{name}` (hostname hợp lệ như validDomain; trùng → 409 với thông điệp chung `"domain không đăng ký được"`), `DELETE /api/domains/{id}` (owner/admin; đang dùng → 409). Ghi audit `domain.register`/`domain.remove` — lời gọi thêm ở Task 11 (chưa có logger ở task này).
  - `{db}` theo tên: tên DB là tên đầy đủ có tiền tố (duy nhất toàn hệ thống) → tra như cũ rồi áp quyền (không thấy → 404).
  - Members: `GET /api/databases/{db}/members`, `POST /api/databases/{db}/members` `{username}`, `DELETE /api/databases/{db}/members/{user_id}` — POST/DELETE chỉ `IsDBOwner` (người quản lý → 403). Username không tồn tại → 400 `"không thêm được"` (giống mọi input sai khác).
  - Destinations: list → `VisibleDestinations`; update/delete/test → 404 nếu không thấy, 403 nếu thấy (shared) nhưng `!CanManage`; create → `OwnerID = p.UserID`, `shared=false`; `PATCH /api/destinations/{id}` `{shared}` chỉ admin. `destJSON` thêm `owner`, `shared`; với destination shared mà người xem không manage: ẩn `endpoint`, `access_key`, `bucket`.
  - `GET /api/destinations/{id}/objects` → `ListRestorable` (trả `{objects:[{key,size,last_modified,database}], truncated}`).
  - Schedules: create/update cần DB access + `CanUseDestination(dest)` (không → 400 "destination not usable"). `PUT /api/schedules/{id}` và `DELETE /api/schedules/{id}`: tải lịch → DB của lịch → `CanAccessDB` (không → **404**, không lộ lịch tồn tại).
  - Backup now: `startedBy = &p.UserID`; có `destination_id` → `CanUse` (không → 400 "destination not usable"); không có → quy tắc fallback của `StartBackup` (Task 5).
  - `getBackup`/`download`: backup thuộc DB có quyền (không → 404).
  - `startRestore`: `CanRestoreKey` (false → 404); `OwnerID = p.UserID`. `getRestore`: admin hoặc `started_by_id == p.UserID` (không → 404).
  - `/api/settings/allowlist` GET/PUT: chỉ admin (403).
  - Superuser (theo từng DB): `POST /api/databases/{db}/superuser/reveal` → `{user, password}`; `POST /api/databases/{db}/superuser/rotate` → 204 — cả hai chỉ owner/admin (người quản lý → 403). Ghi audit `superuser.reveal`/`superuser.rotate` — lời gọi thêm ở Task 11. `dbJSON` thêm `admin_user` (tên superuser, không mật khẩu) chỉ khi người xem là owner/admin, và `legacy: true` khi `admin_user` rỗng.

- [ ] **Step 1: Viết test** (3 user: admin `root1`, `alice`, `bob`; fake runtime/objects; DB `alice_app` của alice)

```go
func TestDBVisibility(t *testing.T)        // alice tạo "app" → "alice_app"; bob GET /api/databases không có alice_app; GET /api/databases/alice_app → 404; admin thấy, owner="alice"
func TestDBListLastBackup(t *testing.T)    // sau một backup ok → list có last_backup.status "ok" và finished_at; DB chưa backup → last_backup null
func TestConnectionMasked(t *testing.T)    // GET /api/databases/alice_app → connection_string chứa "****", không chứa mật khẩu thật; GET …/connection → có mật khẩu; bob (không quyền) GET …/connection → 404
func TestDomainsAPI(t *testing.T)          // alice POST /api/domains "alice.lan" → 201; bob cùng tên → 409 {"error":"domain không đăng ký được"} (không nhắc tới alice); bob GET /api/domains không thấy "alice.lan"; alice tạo DB với domain của bob → 400; DELETE domain đang dùng → 409; admin GET thấy tất cả
func TestMembersAPI(t *testing.T)          // alice thêm bob → bob thấy alice_app, connection_string (che) có; bob POST members carol → 403; bob DELETE chính mình → 403; alice POST members "nobody" → 400 "không thêm được"
func TestDestinationOwnership(t *testing.T)// alice tạo dA; bob list không có, PUT/DELETE/test → 404; admin PATCH shared dAdmin → bob thấy (không có endpoint/bucket/access_key), PUT → 403; user PATCH shared → 403
func TestScheduleDestinationRules(t *testing.T) // bob đặt lịch cho DB của mình bằng dA (riêng alice) → 400; bằng shared → 201
func TestIndirectAccessBlocked(t *testing.T) {
	// trên dS shared có backup alice_app (alice): bob POST /api/restores với key đó → 404; bob GET /api/backups/<id alice_app>/download → 404
	// bob GET /api/destinations/dS/objects không chứa key của alice_app; bob POST /api/databases/alice_app/backups → 404
	// lịch của alice_app: bob PUT /api/schedules/<id> → 404; bob DELETE /api/schedules/<id> → 404; lịch vẫn còn nguyên
	// bob POST /api/databases/bob_x/backups với destination_id = dA (riêng alice) → 400
}
func TestRestoreOwnership(t *testing.T)    // alice restore key alice_app → 202; node mới owner alice; bob GET /api/restores/<id> → 404
func TestAllowlistAdminOnly(t *testing.T)  // alice GET/PUT → 403; admin OK
func TestSuperuserAccess(t *testing.T)     // alice reveal superuser của alice_app → "su_<16 hex>" + mật khẩu; bob (member của alice_app) reveal/rotate/connection?role=superuser → 403, dbJSON của alice_app không có admin_user; admin reveal → OK
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/api/ ./internal/store/...` PASS.
- [ ] **Step 5: Commit** `feat(api): enforce per-resource permissions`.

---

### Task 11: Audit log & Hoạt động

**Files:**
- Create: `internal/store/audit.go` + test, `internal/audit/audit.go` + test, `internal/api/audit_routes.go` + test
- Modify: `internal/api/{api.go,auth_routes.go,admin_routes.go}` (gọi ghi audit), `internal/app/{backup.go,scheduler.go}` (sự kiện `system`), `cmd/fridge/main.go` (job dọn audit)

**Interfaces:**
- Consumes: Task 9 (route auth, `auth.Attempt`, `AddFixed`), Task 10 (route tài nguyên, `/connection`, domains, superuser).

```go
// store
type AuditEntry struct { ID int64; At time.Time; ActorID *int64; Actor, IP, Action, TargetType, TargetID string; DBID *int64; Success bool; Detail map[string]any }
type AuditFilter struct { ActorID, DBID *int64; Actor string /* username, khớp đúng cột actor */; Action string /* tiền tố, vd "db." */; From, To *time.Time; BeforeID int64 /* phân trang lùi */; Limit int /* ≤ 500 */; VisibleTo *int64 /* nil = admin; ngược lại actor_id = user OR db_id thuộc DB user là owner */ }
func (s *Store) InsertAudit(ctx, e AuditEntry) error
func (s *Store) ListAudit(ctx, f AuditFilter) ([]AuditEntry, error) // mới nhất trước
func (s *Store) PurgeAudit(ctx, olderThan time.Time) (int, error)
type Job struct { Kind string /* "backup" | "restore" | "dumpcheck" | "image_pull" */; ID int64; DBName, StartedBy string /* username hoặc "system" */; StartedAt time.Time }
func (s *Store) RunningJobs(ctx) ([]Job, error) // backups/restores/dump_checks đang running; StartedBy từ started_by_id LEFT JOIN users (NULL → "system"); KHÔNG JOIN audit
type OnlineSession struct { Session; Username string }
func (s *Store) OnlineSessions(ctx, since time.Time) ([]OnlineSession, error) // last_seen_at >= since, kèm username
// audit
type Logger struct { Store *store.Store }
func (l *Logger) Record(ctx context.Context, e store.AuditEntry) // không bao giờ trả lỗi; lỗi → log.Printf
func Actor(p auth.Principal) (id *int64, name string)
```
- Các mã hành động (cố định, dùng đúng chuỗi; regex `^[a-z_]+\.[a-z_]+$`, không chữ số): `auth.setup`, `auth.login`, `auth.login_failed`, `auth.logout`, `auth.password_change`, `invite.create`, `invite.accept`, `user.role`, `user.disable`, `user.enable`, `user.reset_password`, `session.revoke`, `db.create`, `db.member_add`, `db.member_remove`, `db.reveal_password`, `superuser.reveal`, `superuser.rotate`, `backup.start`, `backup.download`, `restore.start`, `dumpcheck.start`, `dest.create`, `dest.update`, `dest.delete`, `dest.share`, `dest.test`, `domain.register`, `domain.remove`, `schedule.create`, `schedule.update`, `schedule.delete`, `settings.allowlist`, `settings.endpoint_allowlist`, `image.pull`, `image.prune`, `recover.allowlist`, `recover.reset_password`, `recover.promote`, `system.backup`, `system.retention_delete`.
- Ở task này ghi audit cho mọi route đã có (Task 9, 10), gồm cả `domain.register`/`domain.remove`, `superuser.reveal`/`superuser.rotate`, `db.reveal_password` (trên `GET /api/databases/{db}/connection` đã có từ Task 10). Route của task sau (`dumpcheck.start`, `image.*`, `settings.endpoint_allowlist`, `recover.*`) do chính task đó ghi.
- Ghi ở tầng API sau khi biết kết quả (thành công **và** thất bại vì quyền/validate); `auth.login_failed` dùng `auth.Attempt` từ `LoginStart`/`LoginFinish` (kể cả `ErrLocked`): `att.UserID != nil` → actor = username đó, actor_id = id; ngược lại actor `anonymous` + `detail.username`. Scheduler ghi `system.backup` (kết quả backup) và `system.retention_delete`. **Detail không chứa** mật khẩu, credentials, secret, token.
- API: `GET /api/audit?actor_id=&actor=<username>&action=&db=<name|id>&from=&to=&before=&limit=` → `{entries, next_before}` (`db` tra như `dbOf`: số → id, ngược lại tên đầy đủ; DB không thấy → kết quả rỗng, không 404 lộ tồn tại); `GET /api/audit.csv` (cùng tham số, `Content-Disposition: attachment`, cột: at, actor, ip, action, target_type, target_id, db, success, detail); `GET /api/activity` (admin) → `{jobs, online}` với jobs = `RunningJobs` (Task 15 nối thêm job pull image), online = `OnlineSessions(now − 5 phút)` gồm `username`.
- `main.go`: `a.Sched.AddFixed("23 3 * * *", …PurgeAudit(now − 365 ngày)…)`.

- [ ] **Step 1: Viết test** (admin `root1`, `alice`, `bob`, `carol`)

```go
func TestAuditAppendOnly(t *testing.T)     // UPDATE audit_log → lỗi; DELETE dòng mới → lỗi; DELETE dòng at = now-366 ngày → OK (PurgeAudit trả 1)
func TestAuditVisibility(t *testing.T)     // alice owner alice_app, bob member: bob backup alice_app → alice thấy (db_id alice_app); bob thấy của mình; carol không thấy gì của alice_app; admin thấy hết
func TestAuditFilters(t *testing.T)        // ?actor=bob chỉ dòng của bob; ?db=alice_app và ?db=<id> cho cùng kết quả; ?action=db. lọc tiền tố
func TestAuditRecordedForActions(t *testing.T) // qua API: đăng nhập sai "alice" → auth.login_failed actor "alice"; user không tồn tại → actor "anonymous", detail.username; tạo DB → db.create success; bob gọi /api/databases/alice_app (404) không ghi db.*; bob POST members alice_app (403) → db.member_add success=false; domain đăng ký → domain.register; reveal superuser → superuser.reveal
func TestRevealPasswordAudited(t *testing.T) // GET …/connection → audit có db.reveal_password với actor đúng; GET /api/databases/alice_app (che) không ghi
func TestAuditNoSecrets(t *testing.T)      // sau chuỗi hành động (setup, login, tạo dest với secret "TOPSECRET", tạo DB với mật khẩu "S3cure-pass.~x", đổi mật khẩu): dump toàn bộ audit_log::text không chứa "TOPSECRET", mật khẩu DB, session id, invite token
func TestAuditCSV(t *testing.T)            // header đúng cột; số dòng = số entry theo bộ lọc
func TestActivityAdminOnly(t *testing.T)   // user → 403; admin thấy job running (backup do alice bấm, đang chặn bằng execFn) với started_by "alice"; backup theo lịch → "system"; online gồm 2 phiên kèm username
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/...` PASS.
- [ ] **Step 5: Commit** `feat: append-only audit log and activity view`.

---

### Task 12: Chặn SSRF cho destination

**Files:**
- Create: `internal/objstore/guard.go` + `guard_test.go`, `internal/app/endpoint.go` + test
- Modify: `internal/objstore/objstore.go` (`New` nhận guard), `internal/app/app.go` (`OpenObjects` nhận guard), `internal/app/destinations.go` (`objects()`), `internal/app/fakes_test.go`, `internal/api/api.go` (create/update destination), `internal/api/admin_routes.go` (setting allowlist endpoint)

**Interfaces:**
- Consumes: `Runtime.FridgeSubnets` (Task 8), principal/quyền (Task 9–10), audit logger (Task 11).

```go
// objstore
var ErrEndpointBlocked = errors.New("endpoint không được phép")
var BlockedNames = []string{"fridge", "fridge-meta", "traefik", "fridge-traefik"}
type Guard struct { Blocked []netip.Prefix /* subnet Docker của Fridge */; Allow []string /* host bỏ qua guard, so khớp không phân biệt hoa thường */ }
func (g *Guard) CheckIP(ip netip.Addr) error   // Unmap() trước; loopback, link-local unicast/multicast (169.254.0.0/16, fe80::/10), unspecified, thuộc Blocked → ErrEndpointBlocked
func (g *Guard) CheckHost(ctx context.Context, host string) error // host ∈ Allow → nil; tên ∈ BlockedNames → ErrEndpointBlocked; IP literal → CheckIP; tên → net.DefaultResolver.LookupNetIP, MỌI IP qua CheckIP
func (g *Guard) DialControl(network, address string, c syscall.RawConn) error // cho net.Dialer.Control: parse address (ip:port đã resolve) → CheckIP
func New(d store.Destination, secretKey string, g *Guard) (*Store, error)
	// g nil → như cũ; g != nil: host endpoint ∈ g.Allow → không gắn Control; ngược lại minio.Options.Transport = *http.Transport (clone DefaultTransport)
	// với DialContext của &net.Dialer{Control: g.DialControl} → kiểm IP thật lúc kết nối (chống DNS rebinding)
// app
// App.OpenObjects đổi chữ ký: func(d store.Destination, secretKey string, g *objstore.Guard) (ObjectStore, error)
func (a *App) EndpointAllowlist(ctx) ([]string, error)            // setting "s3_endpoint_allowlist" (JSON []string), chưa có → []
func (a *App) SetEndpointAllowlist(ctx, hosts []string) error      // mỗi host: hostname/IP hợp lệ, không rỗng → ngược lại ErrInvalid
func (a *App) endpointGuard(ctx) (*objstore.Guard, error)          // Blocked = a.RT.FridgeSubnets(ctx); Allow = EndpointAllowlist
func (a *App) CheckEndpoint(ctx, p auth.Principal, endpoint string) error // p.IsAdmin() → nil; ngược lại url.Parse → host → guard.CheckHost; lỗi → ErrInvalid "endpoint không được phép" (không nêu lý do)
```
- `a.objects()`: owner của destination hiện **không phải admin** (`GetUser(owner).Role != "admin"`) → mở objstore với `endpointGuard`; owner là admin → guard nil. (Destination do admin tạo không bị kiểm; admin bị hạ quyền thì destination của họ bắt đầu bị kiểm.)
- API: `POST /api/destinations`, `PUT /api/destinations/{id}` của user không phải admin → `CheckEndpoint` trước khi lưu; lỗi → 400 `{"error":"endpoint không được phép"}` + audit `dest.create`/`dest.update` success=false. `GET|PUT /api/settings/endpoint-allowlist` `{hosts}` — chỉ admin (403); PUT ghi audit `settings.endpoint_allowlist`.

- [ ] **Step 1: Viết test**

```go
func TestGuardCheckIP(t *testing.T)        // Blocked [172.18.0.0/16, 172.19.5.0/24]: chặn 127.0.0.1, ::1, 169.254.169.254, fe80::1, 0.0.0.0, ::, 172.18.0.5, 172.19.5.9, ::ffff:127.0.0.1; cho 192.168.1.50, 10.1.2.3, 172.19.6.1, 8.8.8.8
func TestGuardCheckHostNames(t *testing.T) // "fridge-meta", "TRAEFIK", "fridge" → ErrEndpointBlocked; "localhost" (resolve loopback) → chặn; Allow ["localhost"] → "localhost" OK
func TestDialGuardBlocksAtConnect(t *testing.T) // httptest server 127.0.0.1; http.Client dùng transport có Dialer.Control = (&Guard{}).DialControl → request lỗi, errors.Is(err, ErrEndpointBlocked) — kiểm ở lúc dial, độc lập với kiểm lúc tạo
func TestObjstoreNewWithGuard(t *testing.T) // objstore.New(dest endpoint "http://127.0.0.1:<port httptest>", g) → Test(ctx) lỗi chứa "endpoint không được phép"; g nil → tới được server
func TestCheckEndpointRoles(t *testing.T)  // app: admin + "http://127.0.0.1:9000" → nil; alice → ErrInvalid "endpoint không được phép"; SetEndpointAllowlist(["127.0.0.1"]) → alice OK; "http://192.168.1.50:9000" → OK
func TestObjectsUsesGuardForNonAdminOwner(t *testing.T) // fake OpenObjects ghi g: destination của alice → g != nil, g.Blocked == FridgeSubnets của fake runtime; destination của admin → g == nil
func TestDestinationSSRFAPI(t *testing.T)  // bob POST endpoint "http://fridge-meta:9000", "http://127.0.0.1:9000", "http://169.254.169.254" → 400 {"error":"endpoint không được phép"}; "http://192.168.1.50:9000" → 201; admin "http://127.0.0.1:9000" → 201; bob PUT /api/settings/endpoint-allowlist → 403
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/objstore/ ./internal/app/ ./internal/api/` PASS.
- [ ] **Step 5: Commit** `feat: SSRF guard for user-created S3 destinations`.

---

### Task 13: Validate dump — mức 1 (kiểm tra phần đầu file trước restore)

**Files:**
- Create: `internal/dumpcheck/dumpcheck.go`, `dumpcheck_test.go`, `internal/dumpcheck/testdata/` (dump thật: `pg14.dump`, `pg17.dump` sinh bằng `pg_dump -Fc` từ container `postgres:14`/`postgres:17` chạy pg_dump cùng major, `pg13-by-pgdump17.dump` = server 13 dump bằng pg_dump 17, `plain.sql.gz`, `not-a-dump.dump` (một PNG nhỏ), `empty.dump`)
- Modify: `internal/app/app.go` (ObjectStore thêm `ReadHead`), `internal/objstore/objstore.go`, `internal/app/restore.go`, fakes

**Interfaces:**
```go
// dumpcheck
type Header struct { Format string /* "custom" | "sql" */; Gzip bool; ArchiveVersion, ServerVersion, PgDumpVersion string }
func Inspect(key string, head []byte) (Header, error)
	// rỗng → lỗi "empty file"; ".dump" phải bắt đầu "PGDMP" và format byte = custom, parse theo ReadHead() của PostgreSQL
	//   (src/bin/pg_dump/pg_backup_archiver.c; archive 1.12–1.16; int = 1 byte dấu + intSize byte little-endian; string = int độ dài + byte; với ≥1.15 có byte thuật toán nén)
	// ".sql.gz" → header gzip, giải nén phần có được; ".sql"/".sql.gz" → UTF-8 hợp lệ (cho phép rune bị cắt ở cuối), không NUL, có "-- PostgreSQL database dump" hoặc dòng bắt đầu SET|CREATE|COPY|INSERT|BEGIN
	// đuôi khác → lỗi "unsupported file type"
func Major(version string) int // "17.2" → 17; "14beta1" → 14
const HeadSize = 64 << 10

// objstore + app.ObjectStore
ReadHead(ctx context.Context, key string, n int) ([]byte, error) // GetObject với SetRange(0, n-1)
```
- `StartRestore` (phần đồng bộ, **trước** khi tạo row/node): `ReadHead(key, HeadSize)` + `Inspect`; lỗi → `ErrInvalid` với lý do. Với `.dump`: `ServerVersion` ghi đè version nguồn cho `CheckCompat`; `Major(PgDumpVersion) > Major(target version)` → `ErrInvalid` "dump made by pg_dump X cannot be restored by pg_restore Y".

- [ ] **Step 1: Tạo testdata** bằng script trong WSL (container postgres tạm, bảng mẫu vài dòng; commit file nhị phân, mỗi file < 50 KB). **Viết test:**

```go
func TestInspectCustom(t *testing.T)   // pg14.dump → Format custom, Major(ServerVersion)==14, Major(PgDumpVersion)==14; pg17.dump → 17
func TestInspectPgDumpNewer(t *testing.T) // pg13-by-pgdump17.dump → server 13, pg_dump 17
func TestInspectRejects(t *testing.T)  // empty.dump → "empty"; not-a-dump.dump → "not a pg_dump custom-format"; "x.zip" → "unsupported"; "x.sql" với byte NUL → lỗi
func TestInspectSQL(t *testing.T)      // plain.sql.gz (64KB đầu) → Format sql, Gzip true; head bị cắt giữa rune UTF-8 vẫn OK
// app
func TestRestoreHeaderChecks(t *testing.T) // fakeObjects chứa các file testdata: not-a-dump → ErrInvalid, không có Run, không có restore row; pg17.dump → node v16 → ErrInvalid; pg13-by-pgdump17.dump → node v16 → ErrInvalid (pg_dump); pg14.dump → node 17 OK
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/dumpcheck/ ./internal/app/ ./internal/objstore/` PASS.
- [ ] **Step 5: Commit** `feat: validate dump header before restore`.

---

### Task 14: Validate dump — mức 2 ("Kiểm tra kỹ")

**Files:**
- Create: `internal/app/dumpchecks.go` + test, `internal/app/dumpcheck_integration_test.go` (`//go:build integration`)
- Modify: `internal/container/docker.go` (RunSpec thêm `Cmd []string`, `NoNetwork bool`), `internal/api/admin_routes.go`, fakes

**Interfaces:**
- Consumes: `dumpcheck` (Task 13), store `DumpCheck` (Task 2), `RunSpec.Network` (Task 8), `CanRestoreKey` (Task 5), audit logger (Task 11).

```go
// container
type RunSpec struct { /* … */; Cmd []string; NoNetwork bool } // NoNetwork → NetworkMode "none", bỏ qua Network/alias
// app
func (a *App) StartDumpCheck(ctx, destID int64, key string, startedBy int64) (store.DumpCheck, error) // đi qua a.objects() (destination đã xóa → ErrInvalid, guard SSRF Task 12)
func (a *App) GetDumpCheck(ctx, id int64) (store.DumpCheck, error)
```
- Chạy nền (`a.wg`, timeout 6h):
  - `.dump` → container tạm `fridge-check-<id>` image `postgres:<version cao nhất catalog>`, `Cmd ["sleep","infinity"]`, **`NoNetwork: true`**, rồi `Exec pg_restore -v -f /dev/null` với stdin là object; exit ≠ 0 → failed + stderr; đếm dòng stderr chứa `processing item` → `toc_entries` (best-effort); luôn `Remove` container (ctx detached).
  - `.sql.gz`/`.sql` → kiểm **trong Go**, không container (gunzip hết + `utf8` từng khối, xử lý rune vắt qua ranh giới).
  - `result`: `{format, bytes, source_version, pg_dump_version, toc_entries}`.
- API: `POST /api/dump-checks` `{destination_id, key}` → 202 (quyền: `CanRestoreKey`, không → 404); `GET /api/dump-checks/{id}` (admin hoặc `started_by_id`, không → 404). POST **ghi audit `dumpcheck.start`** (thành công và thất bại).

- [ ] **Step 1: Viết test**

```go
func TestDumpCheckSQLGzTruncated(t *testing.T) // .sql.gz bị cắt nửa → check failed, error chứa "unexpected EOF"; nguyên vẹn → ok, bytes đúng; không có Run nào (kiểm trong Go)
func TestDumpCheckDumpUsesTempContainer(t *testing.T) // fakeRuntime: Run với NoNetwork true, Cmd sleep; Exec pg_restore nhận stdin; exit 1 → failed; Remove luôn được gọi
func TestDumpCheckAPI(t *testing.T)        // bob POST với key backup của alice trên dS → 404; alice → 202, GET → trạng thái; bob GET check của alice → 404; audit có dumpcheck.start
```
```go
// integration (//go:build integration, internal/app): TestDumpCheckRealPgRestore — pg14.dump nguyên → ok, toc_entries > 0; cắt đôi → failed; container fridge-check-* không còn
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/app/ ./internal/api/ ./internal/container/` + `go test -tags integration ./internal/app/ -run DumpCheckReal -v` PASS.
- [ ] **Step 5: Commit** `feat: deep dump check job ("Kiểm tra kỹ")`.

---

### Task 15: Trang Images — dữ liệu & API

**Files:**
- Create: `internal/container/images.go`, `internal/registry/registry.go` + test, `internal/app/images.go` + test
- Modify: `internal/app/app.go` (interface `Images` riêng), `internal/api/admin_routes.go`, `internal/api/audit_routes.go` (`/api/activity` nối job pull), `cmd/fridge/main.go`

**Interfaces:**
- Consumes: audit logger + `/api/activity` (Task 11), `AddFixed` (Task 9), `store.Job` (Task 11).

```go
// container (Client thỏa app.Images)
type ImageInfo struct { Ref, ID string; RepoDigests []string; Size int64; Created time.Time; Env []string }
type ContainerUse struct { Name, Image, ImageID string; DBID string; Created time.Time; StartedAt time.Time }
func (c *Client) ImageInspect(ctx, ref string) (ImageInfo, error)   // không có → ErrImageNotFound
func (c *Client) ImagePull(ctx, ref string) error                   // đọc hết stream
func (c *Client) ImageRemove(ctx, ref string) error
func (c *Client) DBContainers(ctx) ([]ContainerUse, error)          // all=true, filter label fridge.db_id
func (c *Client) RemoteDigest(ctx, ref string) (string, error)      // DistributionInspect
// registry
type Client struct { BaseURL, AuthURL string; HTTP *http.Client } // mặc định https://registry-1.docker.io, https://auth.docker.io
func (c *Client) RemoteEnv(ctx, ref, key string) (string, error)  // token ẩn danh (scope repository:library/<name>:pull) → manifest list (Accept OCI index + Docker manifest list) → manifest linux/<runtime.GOARCH> → config blob → giá trị biến env `key`
// app
type ImageRow struct { Ref, ShortDigest, Version string; Size int64; Created time.Time; Present bool; UsedBy []string; LastUsed *time.Time; Update *store.ImageCheck }
func (a *App) ListImages(ctx) ([]ImageRow, error)            // catalog refs ∪ image của DBContainers; Version từ env PG_VERSION
func (a *App) PullImages(refs []string, startedBy string) (jobID string, err error) // ref phải thuộc catalog → ErrInvalid; job RAM
func (a *App) PullStatus(jobID string) (map[string]string, error) // ref → "pulling|done|error: …"
func (a *App) RunningPulls() []store.Job                      // job pull đang chạy: Kind "image_pull", DBName = ref, StartedBy = username, StartedAt
func (a *App) PrunePreview(ctx) ([]ImageRow, int64, error)    // Present && len(UsedBy)==0
func (a *App) Prune(ctx, refs []string) ([]string, error)     // kiểm lại ngay trước khi xóa; ref đang dùng/không trong preview → bỏ qua; trả các ref đã xóa
func (a *App) CheckImageUpdates(ctx) error                    // mỗi image catalog có mặt: RemoteDigest ≠ local RepoDigest → RemoteEnv(PG_VERSION) → UpsertImageCheck; lỗi mạng → ghi Error, không trả lỗi tổng
```
- `App` thêm field `Img Images` (interface gồm các method container ở trên) và `Registry interface{ RemoteEnv(...) }`; nil trong test cũ.
- `main.go`: `a.Sched.AddFixed("17 4 * * *", …CheckImageUpdates…)`.
- API (admin, user → 403): `GET /api/images`, `POST /api/images/pull` `{refs}` → `{job}` (**ghi audit `image.pull`**), `GET /api/images/pull/{job}`, `GET /api/images/prune` (preview `{images, bytes}`), `POST /api/images/prune` `{refs}` (**ghi audit `image.prune`**, detail = các ref đã xóa), `POST /api/images/check-updates`.
- `GET /api/activity`: `jobs` = `Store.RunningJobs` ∪ `a.RunningPulls()`, sắp theo `StartedAt`.

- [ ] **Step 1: Viết test**

```go
func TestRegistryRemoteEnv(t *testing.T)  // httptest giả auth + registry: token → manifest list có amd64/arm64 → config env ["PG_VERSION=18.2"] → "18.2"; 401 không token → lỗi
func TestListImagesUsage(t *testing.T)    // fake Images: postgres:16 có mặt, 2 container DB dùng (1 dừng) → UsedBy 2 tên, LastUsed = StartedAt mới nhất; postgres:15 chưa có → Present false
func TestPrune(t *testing.T)              // preview chỉ image không dùng; Prune với ref đang dùng → bỏ qua, không gọi ImageRemove; ref tag lạ ngoài catalog → bỏ qua
func TestPullValidation(t *testing.T)     // "nginx:latest" → ErrInvalid; "postgres:15" → job, status done; trong lúc pull (fake chặn) RunningPulls có 1 job Kind "image_pull"
func TestCheckImageUpdates(t *testing.T)  // local digest khác remote → ImageCheck RemoteVersion "18.2", LocalVersion "18.1"; registry lỗi → Error ghi lại, hàm trả nil
func TestImagesAdminOnly(t *testing.T)    // api: user → 403; admin pull → audit image.pull; prune → audit image.prune
func TestActivityIncludesPulls(t *testing.T) // pull đang chạy → GET /api/activity jobs chứa kind "image_pull" với started_by admin
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** PASS (+ `go test -tags integration ./internal/container/` thêm case ImageInspect/DBContainers thật).
- [ ] **Step 5: Commit** `feat: images management (pull, prune, usage, update check)`.

---

### Task 16: HTTPS + deploy (Postgres 18, Traefik container cố định)

**Files:**
- Modify: `internal/pki/ca.go` (+ test), `internal/proxy/traefik.go` (+ test), `internal/app/app.go` (`WebCert, WebKey string`; `SetAllowlist` truyền vào `WriteBase`), `cmd/fridge/main.go`, `deploy/traefik.yml`, `deploy/docker-compose.yml`, `README.md`
- Create: `deploy/.env.example` (**file mới** — hiện chưa có trong git; không chứa secret thật), `cmd/fridge/webcert.go` (+ test)

**Interfaces:**
- Consumes: `AddFixed` (Task 9), `ReconcileNetworks`/`FRIDGE_TRAEFIK_CONTAINER` (Task 8).

```go
func (c *CA) IssueNames(names []string, validFor time.Duration) (certPEM, keyPEM []byte, err error) // tên là IP → IPAddresses, còn lại → DNSNames; Issue(domain) gọi IssueNames([]string{domain})
func ensureWebCert(ca *pki.CA, certFile, keyFile string, hosts []string) error // SAN = localhost, 127.0.0.1 + hosts; sinh lại khi SAN khác hoặc còn < 30 ngày; hạn 365 ngày; ghi file mới bằng rename nguyên tử
func WriteBase(dir string, cidrs []string, apiBackend, webCert, webKey string) error // webCert/webKey bắt buộc khác rỗng → ngược lại lỗi
```
- `WriteBase` tạo: router HTTP `fridge-web` (entryPoints `[websecure]`, `PathPrefix(`/`)`, `tls: {}`, middleware `allowlist-http@file`, service `fridge-api`); router `fridge-redirect` (entryPoints `[web]`, `PathPrefix(`/`)`, middleware `to-https@file`, service `fridge-api`) với middleware `to-https: redirectScheme {scheme: https, port: "8443", permanent: true}`; `tls.stores.default.defaultCertificate {certFile: webCert, keyFile: webKey}`. Giữ middleware TCP.
- Cert web ở `<FRIDGE_TRAEFIK_DIR>/certs/web.crt|web.key`; `main` gọi `ensureWebCert` trước `WriteBase` (env `FRIDGE_PUBLIC_HOSTS` tách dấu phẩy, trim) và gán `a.WebCert/a.WebKey`.
- **Gia hạn cert web**: `a.Sched.AddFixed("41 3 * * *", func(){ ensureWebCert(…) })` (lỗi → log + alert); Traefik tự nạp lại file đã ghi đè nhờ file provider `watch`.
- `traefik.yml`: entryPoints `web: ":8080"`, `websecure: ":8443"`, `pg: ":5432"`, `ping: ":8082"`; file provider giữ `watch: true`.
- Compose:
  - `traefik`: `container_name: fridge-traefik`; publish `8080`, `8443`, `5432`; network `fridge-net`.
  - `fridge-meta`: image **`postgres:18`**, volume `fridge-meta:/var/lib/postgresql` (không phải `/var/lib/postgresql/data`).
  - `fridge`: env bỏ `FRIDGE_TOKEN`, thêm `FRIDGE_SETUP_TOKEN: ${FRIDGE_SETUP_TOKEN:-}`, `FRIDGE_PUBLIC_HOSTS: ${FRIDGE_PUBLIC_HOSTS:-}`, `FRIDGE_TRAEFIK_CONTAINER: fridge-traefik`, `FRIDGE_TRUSTED_PROXY: ${FRIDGE_TRUSTED_PROXY:-}` (rỗng = subnet `fridge-net`).
  - `fridge-net` chỉ có `traefik`, `fridge`, `fridge-meta` (+ `minio`, `minio-init` profile dev); container DB do Fridge tạo trên network riêng.
- `deploy/.env.example` (tạo mới): `FRIDGE_META_PASSWORD=`, `FRIDGE_SECRET_KEY=` (cách sinh), `FRIDGE_SETUP_TOKEN=` (giải thích: chỉ dùng lần tạo admin đầu tiên, `openssl rand -hex 32`), `FRIDGE_PUBLIC_HOSTS=` (ví dụ `fridge.lan,192.168.1.10`), `# FRIDGE_TRUSTED_PROXY=` (CIDR, để trống = subnet fridge-net), `FRIDGE_ALERT_WEBHOOK=`, `TZ=UTC`. **Không chứa secret thật; không có `FRIDGE_TOKEN`.**
- README: truy cập `https://localhost:8443`, setup lần đầu, cài CA, `fridge login`, các lệnh `recover`, cô lập mạng node; **ghi chú nâng cấp Postgres**: volume `fridge-meta` tạo bởi Postgres 17 không mở được bằng 18 — với dữ liệu dev/smoke thì xóa volume (`docker compose down` + `docker volume rm fridge_fridge-meta`); dữ liệu thật cần dump/restore (hiện chưa có dữ liệu production).

- [ ] **Step 1: Viết test**

```go
func TestIssueNames(t *testing.T)          // ["localhost","127.0.0.1","fridge.lan"] → DNSNames [localhost fridge.lan], IPAddresses [127.0.0.1]; verify với CA
func TestEnsureWebCert(t *testing.T)       // lần 1 tạo; lần 2 cùng hosts → không đổi file (mtime/nội dung); hosts khác → sinh lại; cert còn 29 ngày → sinh lại
func TestWriteBaseHTTPS(t *testing.T)      // yaml: fridge-web entryPoints [websecure] có tls; fridge-redirect dùng to-https; redirectScheme port "8443"; tls.stores.default.defaultCertificate.certFile == webCert; webCert rỗng → lỗi
func TestSetAllowlistKeepsWebCert(t *testing.T) // app.SetAllowlist với WebCert/WebKey đặt → _base.yml vẫn có defaultCertificate
```
- [ ] **Step 2:** FAIL. **Step 3:** implement + file deploy + README. **Step 4:** `go test ./...` PASS; `docker compose -f deploy/docker-compose.yml --env-file deploy/.env.example config -q` OK (điền giá trị giả cho biến bắt buộc qua file tạm nếu cần); `grep -c FRIDGE_TOKEN deploy/docker-compose.yml deploy/.env.example` = 0.
- [ ] **Step 5: Commit** `feat(deploy): HTTPS on :8443, Postgres 18 metadata, fixed Traefik container`.

---

### Task 17: CLI — login, CA TOFU, session, setup, recover

**Files:** Modify `cmd/fridge/cli.go`, `cmd/fridge/main.go`; Create `cmd/fridge/login.go`, `cmd/fridge/recover.go` + tests

**Interfaces:**
- Consumes: API Task 9 (auth routes), `scram` Task 3, `auth` Task 4, `app`/`proxy`/`store` (recover), audit logger Task 11, cert web Task 16.
- Produces lệnh:
  - `fridge login [--user U] [--password-stdin] [--yes] [--ca-file F]`: lần đầu chưa có CA → GET `/api/ca` (chỉ request này bỏ qua kiểm TLS), in `SHA256 fingerprint: AA:BB:…`, hỏi `Trust this CA? [y/N]` (`--yes` bỏ qua hỏi), lưu `~/.config/fridge/ca.pem`; SCRAM qua `/api/auth/scram/*` với `want_token:true`, `Verify` server signature; lưu `~/.config/fridge/session` (JSON `{url, session}`, 0600). Nhập mật khẩu không echo bằng `stty -echo` (lỗi → cảnh báo, đọc có echo).
  - `fridge logout`, `fridge whoami`.
  - `fridge setup --setup-token T --user U [--password-stdin]` (cho server không có trình duyệt; đăng ký Credentials tính bằng `scram`). *(Lựa chọn của Claude, ghi trong spec mục 12.)*
  - Mọi request: `FRIDGE_URL` mặc định `https://localhost:8443`; session = env **`FRIDGE_SESSION`** nếu có, ngược lại từ file session; TLS tin CA từ `--ca-file`/`FRIDGE_CA_FILE`/`ca.pem` đã lưu + system pool; **không** có cờ tắt kiểm TLS. 401 → in `not logged in — run: fridge login`; `setup required` → in hướng dẫn mở web hoặc `fridge setup`.
  - `fridge recover allowlist <cidr>... | reset-password <user> | promote <user>`: chạy **trong container**, đọc env server (`FRIDGE_DATABASE_URL`, `FRIDGE_SECRET_KEY`, `FRIDGE_TRAEFIK_DIR`, `FRIDGE_API_BACKEND`, `FRIDGE_PUBLIC_HOSTS`), thao tác thẳng store/proxy:
    - allowlist → dựng `app.App{Store, TraefikDir, APIBackend, WebCert: <FRIDGE_TRAEFIK_DIR>/certs/web.crt, WebKey: <FRIDGE_TRAEFIK_DIR>/certs/web.key}` rồi gọi `app.SetAllowlist` (không bao giờ gọi `WriteBase` thiếu đường dẫn cert);
    - reset → `auth.CreateResetInvite(nil, id)` in `https://<host đầu tiên trong FRIDGE_PUBLIC_HOSTS hoặc localhost>:8443/#/invite/<token>`;
    - promote → `auth.Promote`.
    - Mỗi lệnh **ghi audit** actor `recover` (`recover.allowlist`, `recover.reset_password`, `recover.promote`).

- [ ] **Step 1: Viết test**

```go
func TestLoginTOFU(t *testing.T)        // httptest TLS server ký bằng CA test: --yes → ca.pem lưu, fingerprint in ra; lần 2 không hỏi; server đổi CA khác → lỗi TLS (không tự tin lại)
func TestLoginSCRAMSavesSession(t *testing.T) // fake API scram (dùng scram.StartServer) → file session 0600 chứa session; whoami gửi Bearer
func TestSessionEnvOverrides(t *testing.T)   // FRIDGE_SESSION=xyz → request gửi "Bearer xyz" dù file session có giá trị khác
func TestNotLoggedInMessage(t *testing.T) // 401 → stderr "fridge login", exit 1
func TestSetupCommand(t *testing.T)     // fake API: body POST /api/setup có setup_token, username "alice", credentials (iterations 600000, salt 16 byte), không có mật khẩu thô
func TestRecoverAllowlist(t *testing.T)  // store thật + thư mục traefik tạm (có certs/web.crt|key): recover allowlist 10.0.0.0/8 → setting cập nhật, _base.yml có CIDR và defaultCertificate certFile = <dir>/certs/web.crt; audit có recover.allowlist actor "recover"
func TestRecoverResetAndPromote(t *testing.T) // in link chứa "/#/invite/"; promote "bob" → role admin; audit recover.reset_password, recover.promote
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./cmd/fridge/` + `go test -race ./cmd/fridge/` PASS.
- [ ] **Step 5: Commit** `feat(cli): session login with CA TOFU, setup and recover`.

---

### Task 18: CLI — lệnh tài nguyên (check, members, share, domains, superuser, show, audit, credentials)

**Files:** Modify `cmd/fridge/cli.go`; Create `cmd/fridge/resources.go` + `resources_test.go`

**Interfaces:**
- Consumes: client HTTP + session của Task 17; API Task 10–15.
- Produces lệnh (mỗi lệnh gọi đúng route, in kết quả dạng bảng/JSON như lệnh cũ):
  - `fridge check --from <dest>:<key> [--wait]` → `POST /api/dump-checks`, `--wait` poll `GET /api/dump-checks/{id}` mỗi 2s.
  - `fridge members ls|add|rm <db> [user]` → `/api/databases/{db}/members`.
  - `fridge dest share <name|id> on|off` → `PATCH /api/destinations/{id}` `{shared}`.
  - `fridge domains add <name>`, `fridge domains ls`, `fridge domains rm <name>` → `/api/domains`.
  - `fridge create <engine> <hậu tố>` (in ra tên đầy đủ `<username>_<hậu tố>`), `--domain` phải là domain đã đăng ký.
  - `fridge create` và `fridge restore` thêm `--db-user U`, `--db-password-stdin`, `--random-credentials` (`--db-user` là hậu tố; sinh hậu tố 8 ký tự `[a-z0-9]`, mật khẩu 24 ký tự `[A-Za-z0-9-_.~]` bằng `crypto/rand`, in ra một lần); `--random-credentials` loại trừ hai cờ kia.
  - `fridge show <db> [--reveal]` — user ứng dụng; `--reveal` gọi `GET /api/databases/{db}/connection` (có ghi audit), không có `--reveal` in chuỗi đã che. Không có cờ `--superuser`.
  - `fridge superuser show <db> [--reveal]` (không `--reveal`: chỉ in tên `admin_user`; có: `POST …/superuser/reveal`), `fridge superuser rotate <db>` (`POST …/superuser/rotate`).
  - `fridge audit [--user U] [--action A] [--db D] [--since 24h] [--csv]` → `GET /api/audit` (`actor=U`, `db=D`, `from=now-since`) hoặc `/api/audit.csv`.

- [ ] **Step 1: Viết test** (httptest fake API ghi lại method/path/body)

```go
func TestCheckCommand(t *testing.T)      // check --from dS:fridge/x.dump → POST /api/dump-checks {destination_id, key}; --wait poll tới ok
func TestMembersCommands(t *testing.T)   // members add alice_app bob → POST /api/databases/alice_app/members {"username":"bob"}; rm → DELETE …/members/<id>
func TestDomainsCommands(t *testing.T)   // add/ls/rm đúng route
func TestShowAndSuperuser(t *testing.T)  // show alice_app → GET /api/databases/alice_app (không gọi /connection); show --reveal → GET …/connection; superuser show → không gọi reveal; superuser show --reveal → POST …/superuser/reveal; superuser rotate → POST …/superuser/rotate
func TestAuditCommand(t *testing.T)      // audit --user bob --db alice_app --since 24h → query actor=bob&db=alice_app&from=…; --csv → /api/audit.csv
func TestCredentialFlags(t *testing.T)   // --random-credentials cùng --db-user → lỗi; --random-credentials → body db_user 8 ký tự [a-z0-9], db_password 24 ký tự; --db-password-stdin đọc stdin
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./cmd/fridge/` PASS.
- [ ] **Step 5: Commit** `feat(cli): members, share, domains, superuser, show, audit and check commands`.

---

### Task 19: Web — khung, SCRAM JS, setup/đăng nhập/lời mời/đổi mật khẩu

**Files:**
- Create: `internal/web/web.go` + test, `internal/web/static/index.html`, `static/css/app.css`, `static/js/{app.js,api.js,scram.js,router.js,layout.js,ui.js}`, `static/js/pages/{setup.js,login.js,invite.js,password.js}`, `static/test/scram.html`, `static/js/test-scram.js`, `static/vendor/bootstrap/…` (5.3.3: `bootstrap.min.css`, `bootstrap.bundle.min.js`), `static/vendor/bootstrap-icons/…` (1.11.3: css + fonts), `static/vendor/VERSIONS.txt`, `scripts/ui-smoke/smoke.mjs`, `scripts/ui-smoke/run.sh`
- Modify: `cmd/fridge/main.go` (truyền `Options.Web = web.Handler()`)

**Interfaces:**
- Consumes: API Task 9.

```go
func Handler() http.Handler // phục vụ embed static/; "/" → index.html; file không có → 404; Cache-Control: no-cache cho index.html, max-age=86400 cho /vendor/
```
```js
// scram.js (ES module, Web Crypto)
export async function makeCredentials(password)          // → {salt, iterations: 600000, stored_key, server_key} (base64)
export async function scramLogin(username, password, startURL, finishURL, extra) // start → finish, kiểm ServerSignature, trả JSON finish
export async function clientProof(...)                     // dùng nội bộ + trang test
// api.js
export async function api(method, path, body)  // fetch same-origin, JSON; 401 "setup required" → #/setup; 401 khác → #/login; ném Error(message) cho lỗi khác
// router.js: hash routes "#/setup", "#/login", "#/invite/:token", "#/password", "#/databases", …; route chưa có trang → "Đang xây dựng"
// layout.js: sidebar trái đóng/mở (nút ☰), thu gọn chỉ còn icon, nhớ localStorage "fridge.sidebar"; mục Databases · Restore · Destinations · Domains · Audit log · Settings; admin thêm Users · Sessions · Images · Hoạt động (dựa vào /api/me); dưới: username · Đổi mật khẩu · Đăng xuất
```
- Không inline `<script>`/`style=`/`onclick` (CSP). Ngôn ngữ giao diện: tiếng Việt.
- Trang setup: gọi `GET /api/setup/status`; `token_configured=false` → hiện cảnh báo **"chưa cấu hình setup token"** (đặt `FRIDGE_SETUP_TOKEN` rồi khởi động lại) và khóa form. Form: setup token, username (kiểm `^[a-z][a-z0-9]{2,15}$` trước khi gửi, gợi ý "chữ thường + số, 3–16 ký tự"), mật khẩu ×2 (≥ 12 ký tự, trùng nhau) → `makeCredentials` → `POST /api/setup` → tự đăng nhập.
- Login: username/mật khẩu → `scramLogin`. Invite: lấy token từ hash `#/invite/<token>` → `POST /api/invites/lookup {token}` hiện username/purpose; đặt mật khẩu → `POST /api/invites/accept {token, credentials}` (token không bao giờ nằm trong URL request). Đổi mật khẩu: mật khẩu cũ (reauth) + mới ×2.
- `/test/scram.html`: chạy vector RFC 7677 (user/pencil, salt `W22ZaJ0SNY7soEsUEjb6gQ==`, i=4096, nonces như Task 3) và in `PASS`/`FAIL` vào `<pre id="result">`.

- [ ] **Step 1: Viết test Go**

```go
func TestHandlerServesIndexAndVendor(t *testing.T) // "/" → 200 text/html; "/vendor/bootstrap/bootstrap.min.css" → 200 text/css; "/nope.js" → 404
func TestNoInlineScripts(t *testing.T)  // quét mọi .html trong embed: không có "<script>" có nội dung inline, không có thuộc tính on*= hay style=
```
- [ ] **Step 2:** FAIL. **Step 3:** implement (tải Bootstrap/Icons đúng phiên bản từ jsDelivr vào repo một lần; ghi phiên bản trong `static/vendor/VERSIONS.txt`).
- [ ] **Step 4: Kiểm tra trình duyệt thật bằng Playwright trong Docker** (công cụ kiểm thử, không phải dependency của dự án, không phải build step của frontend):
  - `scripts/ui-smoke/run.sh` chạy `docker run --rm --network host -v "$PWD/scripts/ui-smoke:/work" -w /work mcr.microsoft.com/playwright:v1.49.0-noble sh -c 'npm i --no-save playwright@1.49.0 >/dev/null && node smoke.mjs'` với env `BASE=https://localhost:8443`, `SETUP_TOKEN`, `ADMIN_PW`. Browser mở với `ignoreHTTPSErrors: true`.
  - `smoke.mjs` (phần của Task 19): mở `/test/scram.html` → `#result` chứa `PASS`; mở `/` trên DB trống → thấy form setup; điền setup token + admin `root1` → vào Databases; đăng xuất → đăng nhập lại; **gom mọi lỗi console/`securitypolicyviolation`** → có lỗi thì exit 1. In từng bước PASS/FAIL.
  - Chạy trên stack compose riêng (project `fridge-ui`, volume mới); ghi output vào report; dọn project.
- [ ] **Step 5:** `go test ./internal/web/ ./cmd/fridge/` PASS. **Commit** `feat(web): admin shell, SCRAM in browser, setup/login/invite/password`.

---

### Task 20: Web — Databases, chi tiết DB, Restore, Destinations, Domains

**Files:** Create `internal/web/static/js/pages/{databases.js,database.js,restore.js,destinations.js,domains.js}`; Modify `static/js/router.js` (route `#/databases`, `#/databases/:name`, `#/restore`, `#/destinations`, `#/domains`), `static/js/layout.js` (mục Domains), `static/css/app.css`, `scripts/ui-smoke/smoke.mjs`

**Interfaces:** chỉ dùng API Task 9–14 qua `api.js`.
- **Databases**: bảng (tên, engine/version, domain, trạng thái màu, backup gần nhất từ `last_backup {status, finished_at}`, cột owner nếu admin), nút "+ Tạo DB" (modal: engine, version dropdown từ `/api/engines`, **tên** (input-group: tiền tố cố định `<username>_` + ô hậu tố `^[a-z0-9][a-z0-9-]{0,29}$`), **domain = dropdown các domain đã đăng ký của mình** (rỗng → link sang trang Domains), **User** (input-group: tiền tố cố định `<username>_` + ô hậu tố `^[a-z0-9_]{1,15}$`) + **Mật khẩu** tùy chọn — mỗi ô có nút 🎲 **Ngẫu nhiên** (`crypto.getRandomValues`: hậu tố 8 `[a-z0-9]`, mật khẩu 24 `[A-Za-z0-9-_.~]`), ô mật khẩu có Hiện/Ẩn + Copy, kiểm tra cùng quy tắc với server trước khi gửi; để trống = server tự sinh) → sau tạo chuyển sang chi tiết. Form restore vào node mới có cùng các ô.
- **Chi tiết DB** (mockup đã duyệt): header + nút **Backup now** (poll trạng thái backup 2–3s); tab **Kết nối** (khối **User ứng dụng** và — chỉ owner/admin — khối **Superuser** `su_<node_key>` với Hiện/Copy (`POST …/superuser/reveal`) và nút **Rotate** (có xác nhận); nhãn "kiểu cũ" nếu `legacy`; connection string che mật khẩu; nút Hiện/Copy gọi `GET /api/databases/{db}/connection` — mỗi lần là một dòng audit; host/user/db, link tải CA `/api/ca`); tab **Backups** (bảng: thời gian, nguồn lịch/tay, destination, dung lượng, trạng thái, Tải · Restore… · Log trong modal); tab **Lịch backup** (CRUD, destination dropdown = destination có thể dùng); tab **Người quản lý** (chỉ owner/admin sửa; người khác chỉ xem; lỗi thêm hiện "không thêm được").
- **Restore**: chọn destination → bảng file (`/objects`, ô tìm kiếm, cột DB) → chọn file → nút **Kiểm tra kỹ** (job, hiện kết quả) và form node mới (tên, domain, version — gợi ý version nguồn) → **Restore** → trang trạng thái polling; lỗi mức 1 hiện ngay dưới form. "Restore…" từ tab Backups mở trang này với file chọn sẵn.
- **Destinations**: bảng của mình (admin: tất cả + owner + công tắc dùng chung); thêm/sửa (secret để trống = giữ)/xóa (xác nhận)/Test; lỗi "endpoint không được phép" hiện dưới ô endpoint.
- **Domains** (`domains.js`): bảng domain của mình (admin: tất cả + cột owner) với số DB đang dùng; thêm domain (lỗi trùng hiển thị "domain không đăng ký được"); xóa (khóa nút khi đang dùng). Mục sidebar "Domains".

- [ ] **Step 1:** Viết trang.
- [ ] **Step 2: Mở rộng `scripts/ui-smoke/smoke.mjs`** (Task 19) với các bước, chạy trên stack compose `fridge-ui` (có MinIO dev):
  - admin đăng ký domain `root1.lan` ở trang Domains; tạo destination MinIO → Test OK; tạo DB bằng nút 🎲 Ngẫu nhiên cho user/mật khẩu → connection string chứa đúng user vừa sinh (có tiền tố `root1_`); DB hiện trong bảng, trạng thái running; Hiện connection string thấy mật khẩu; Backup now → dòng backup `ok`, cột backup gần nhất cập nhật; Restore… → Kiểm tra kỹ → `ok`; restore vào node mới → `ok`.
  - admin mời user `bob` (lấy link từ UI) → mở link trong context trình duyệt mới, đặt mật khẩu, đăng nhập → bob không thấy DB/destination của admin; admin bật **dùng chung** → bob thấy destination đó trong dropdown Lịch backup nhưng không có nút Sửa; bob tạo destination endpoint `http://fridge-meta:5432` → hiện "endpoint không được phép".
  - Không có lỗi console/CSP ở bất kỳ trang nào.
- [ ] **Step 3:** `go test ./...` vẫn PASS (`TestNoInlineScripts`). **Commit** `feat(web): databases, restore, destinations and domains pages`.

---

### Task 21: Web — Settings, Users, Sessions, Images, Audit log, Hoạt động

**Files:** Create `internal/web/static/js/pages/{settings.js,users.js,sessions.js,images.js,audit.js,activity.js}`; Modify `static/js/router.js` (route `#/settings`, `#/users`, `#/sessions`, `#/images`, `#/audit`, `#/activity`), `static/js/layout.js` (mục Audit log, Hoạt động; chấm báo Images khi có bản mới), `scripts/ui-smoke/smoke.mjs`

**Interfaces:** chỉ dùng API Task 9–16 qua `api.js`.
- **Settings**: allowlist (admin; textarea CIDR, lưu), **allowlist endpoint S3** (admin; textarea host, `PUT /api/settings/endpoint-allowlist`), tải CA cert, phiên của tôi (bảng: tạo lúc, hoạt động cuối, IP, UA, nút thu hồi; đánh dấu phiên có `current: true` là "phiên hiện tại", không cho thu hồi từ đây).
- **Users** (admin): bảng user (username, role, trạng thái, tạo lúc), nút **Mời** (username — kiểm `^[a-z][a-z0-9]{2,15}$`, role → hiện link để copy), Khóa/Mở, Nâng/Hạ admin (lỗi admin cuối hiện rõ), **Reset mật khẩu** (hiện link).
- **Sessions** (admin): mọi phiên (kèm username), thu hồi.
- **Audit log** (`audit.js`, mọi user, dữ liệu theo quyền): bảng (thời gian, người, IP, hành động, đối tượng, DB, kết quả, chi tiết rút gọn), bộ lọc người (`actor=<username>`)/hành động/DB (`db=<tên>`)/khoảng thời gian, "Tải thêm" (phân trang `before`), nút **Xuất CSV** theo bộ lọc. Mục sidebar "Audit log".
- **Hoạt động** (`activity.js`, admin): job đang chạy (loại — backup/restore/kiểm tra kỹ/pull image, DB hoặc image, người khởi động, bắt đầu lúc, thời gian đã chạy) + người đang online (username, IP, hoạt động cuối); polling 3s. Mục sidebar "Hoạt động".
- **Images** (`images.js`, admin): bảng (tag, digest rút gọn, dung lượng, ngày tạo, version, số DB đang dùng + danh sách tên, lần dùng cuối, huy hiệu **"New version: 18.1 → 18.2"**); nút Pull (chọn engine+version) / Pull tất cả (poll job), **Kiểm tra cập nhật**, **Prune** (modal xác nhận liệt kê image + tổng dung lượng → xóa). Sidebar mục Images có chấm khi có ít nhất một bản mới.

- [ ] **Step 1:** Viết trang.
- [ ] **Step 2: Mở rộng `smoke.mjs`**: khóa user `bob` → trang của bob (context riêng) bị đưa về đăng nhập ở request kế tiếp; hạ quyền admin duy nhất → thông báo lỗi hiển thị; thu hồi một phiên trong Settings → phiên đó mất, phiên hiện tại có nhãn; Audit log có dòng `db.reveal_password` sau khi bấm Hiện, lọc theo người `root1` chạy, Xuất CSV tải được; Hoạt động hiện backup đang chạy; Images: Pull `postgres:15` → hiện "đã có"; Prune preview không chứa image đang được DB dùng; Kiểm tra cập nhật → mỗi dòng có trạng thái (hoặc "không kiểm tra được"); không lỗi console/CSP.
- [ ] **Step 3:** `go test ./...` PASS. **Commit** `feat(web): settings, users, sessions, images, audit and activity pages`.

---

### Task 22: E2E + smoke giai đoạn 2

**Files:** Create `internal/api/e2e_auth_integration_test.go` (`//go:build integration`); Modify `README.md` nếu smoke phát hiện sai lệch

**Interfaces:** Consumes tất cả (Task 1–21).

- [ ] **Step 1: Viết integration test** (store thật + Docker thật + MinIO `fridge-it-minio`, httptest server của `api.New`, client SCRAM của `scram`; container đóng vai Traefik `fridge-it-traefik` theo `FRIDGE_TRAEFIK_CONTAINER`):

```go
func TestE2EOwnershipFlow(t *testing.T) {
	// setup admin root1 → mời alice, bob → alice, bob accept + login (Bearer)
	// admin PUT /api/settings/endpoint-allowlist với host của MinIO test (vd "localhost" — nếu không guard SSRF sẽ chặn loopback); alice đăng ký domain alice.lan, tạo destination riêng (MinIO) + DB postgres 16 "app" → "alice_app" → Backup now → ok
	// bob: không thấy DB/destination của alice (404); alice thêm bob làm member → bob backup được, bob không thêm member (403)
	// admin tạo dS shared; bob đặt lịch bằng dS (201), bằng dest của alice (400)
	// alice restore bản backup vào node mới v17 → owner alice; bob không xem restore của alice (404)
	// file rác trên dest của alice → POST /api/restores 400 trước khi tạo container (đếm container fridge-postgres-* không đổi)
	// hai user: psql bằng user ứng dụng: `SELECT rolsuper FROM pg_roles WHERE rolname = current_user` → f; CREATE TABLE OK; `COPY (SELECT 1) TO PROGRAM 'id'` → lỗi permission
	// hai DB của alice có superuser khác nhau (su_<node_key>); rotate superuser của một DB → mật khẩu cũ hỏng trên DB đó, DB kia không bị ảnh hưởng; alice và bob cùng đặt hậu tố "shop" → "alice_shop" và "bob_shop" chạy song song
	// cô lập mạng: mỗi node chỉ nằm trên "fridge-node-<node_key>"; từ node alice_shop `pg_isready -h <container bob_shop>` thất bại
	// node restore: bảng đã restore thuộc owner = user ứng dụng (pg_tables.tableowner)
	// dọn: removeNode các node (network node bị xóa), xóa object
}
```
- [ ] **Step 2:** Chạy `go test -tags integration ./internal/api/ -run E2E -v` → PASS.
- [ ] **Step 3: Smoke compose** (WSL, project riêng `fridge-smoke`, volume mới): `up -d --build` với `FRIDGE_SETUP_TOKEN` → `docker exec <fridge-meta> psql -U fridge -c 'SHOW server_version'` bắt đầu `18`; `curl -kI http://localhost:8080` = 301/308 tới `https://localhost:8443`; `openssl s_client -connect localhost:8443` cert có SAN `localhost`; `fridge login` (TOFU) trước setup → báo setup required; `fridge setup` → `fridge login` → `fridge ls`; tạo một DB → `docker network inspect fridge-net` không có container DB; `docker exec <container DB> pg_isready -h fridge-meta -t 3` thất bại; restart fridge → `POST /api/setup` 409 và Traefik vẫn gắn vào network node; `docker exec … /fridge recover allowlist 203.0.113.0/24` rồi `… 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16` → truy cập bị chặn rồi mở lại; `recover reset-password root1` in link; `bash scripts/ui-smoke/run.sh` (toàn bộ smoke UI) → PASS. Ghi lệnh + kết quả vào report. Dọn chỉ project của mình + container/network `fridge-postgres-*`/`fridge-node-*` đã tạo.
- [ ] **Step 4:** `go build ./... && go vet ./... && go test ./...` + `go test -race ./internal/... ./cmd/...` PASS.
- [ ] **Step 5: Commit** `test: phase-2 end-to-end ownership flow and smoke`.
