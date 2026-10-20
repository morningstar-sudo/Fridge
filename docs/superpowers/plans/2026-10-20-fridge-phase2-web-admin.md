# Fridge Giai đoạn 2 — Web admin, tài khoản, phân quyền — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Web admin (Vanilla JS + Bootstrap 5) có tài khoản đăng nhập SCRAM, session stateful, vai trò admin/user, sở hữu DB/destination, HTTPS tự ký, validate dump trước restore và trang quản lý image — CLI dùng cùng session.

**Architecture:** Gói mới `internal/scram` (thuật toán thuần), `internal/auth` (setup, đăng nhập, session, lời mời, user — trạng thái SCRAM/rate-limit trong RAM), `internal/dumpcheck` (parse header dump), `internal/registry` (hỏi Docker Hub), `internal/web` (file tĩnh nhúng). `app` thêm khái niệm owner + helper quyền nhận `auth.Principal`; `api` thêm middleware xác thực và áp quyền trên mọi route. Traefik phục vụ web+API qua `:8443` bằng cert ký từ CA nội bộ.

**Tech Stack:** Go 1.26 stdlib (`crypto/pbkdf2`, `crypto/hmac`, `crypto/sha256`, `embed`), các dependency sẵn có, Bootstrap 5.3.3 + Bootstrap Icons 1.11.3 (vendored), Web Crypto API.

**Spec:** `docs/superpowers/specs/2026-10-20-fridge-web-admin-design.md` (bổ sung cho `docs/superpowers/specs/2026-10-20-fridge-design.md`).

## Global Constraints

- Mọi lệnh go/git/docker chạy trong **WSL Ubuntu-24.04** tại `/mnt/d/Windows/Desktop/code/Fridge` (xem `env.md` trong workspace SDD). Postgres test: `bash scripts/test-db.sh &&` trước lệnh test. Commit message LF.
- **Không thêm dependency Go mới.** Dùng stdlib + các module đã có trong `go.mod`.
- Frontend: **Vanilla JS (ES modules) + Bootstrap 5.3.3 + Bootstrap Icons 1.11.3 vendored** trong `internal/web/static/vendor/`; không CDN, không npm, không build step; **không inline script/style** (CSP `default-src 'self'`).
- SCRAM-SHA-256 theo RFC 5802/7677, chỉ `n,,` (không channel binding). PBKDF2 **600000** vòng khi đăng ký; server nhận `iterations` trong [600000, 10000000], salt đúng **16 byte**.
- Username: `^[a-z][a-z0-9._-]{2,31}$`. Mật khẩu ≥ **12** ký tự (kiểm ở client).
- Session ID 32 byte ngẫu nhiên (base64url, không padding); DB chỉ lưu **SHA-256**. Cookie `fridge_session`: `HttpOnly; Secure; SameSite=Strict; Path=/`. Hết hạn: không hoạt động **12h**, tuyệt đối **7 ngày**; cập nhật `last_seen_at` tối đa 1 lần/phút; dọn session hết hạn mỗi giờ.
- Khóa đăng nhập: **5** lần sai trong **1 phút** theo (username, IP) → khóa **5 phút**. Trạng thái SCRAM dở dang sống **60s**. Lời mời hết hạn **48h**, dùng một lần. Reauth token sống **2 phút**, dùng một lần.
- Tài nguyên không có quyền xem → **404**; thao tác cấm trên tài nguyên thấy được → **403**; trước khi setup → `/api/*` (trừ route công khai) trả **401 `{"error":"setup required"}`**.
- Cổng: Traefik `websecure :8443` (TLS, web + API), `web :8080` (chỉ redirect sang HTTPS :8443), `pg :5432`, `ping :8082`. Fridge vẫn nghe HTTP nội bộ `:8080`, không publish.
- Env: **bỏ `FRIDGE_TOKEN`**; thêm `FRIDGE_SETUP_TOKEN` (cần khi chưa có user), `FRIDGE_PUBLIC_HOSTS` (tùy chọn, phẩy), `FRIDGE_TRUSTED_PROXY` (mặc định `traefik`).
- Thao tác nền dùng **polling 2–3s** (không SSE). Store: chỉ thêm **migration 4** (không sửa migration 1–3).

## Review Focus

1. **Quyền bị vượt qua đường gián tiếp** — restore theo key một bản backup của DB người khác trên destination dùng chung; tải backup của DB không có quyền; đặt lịch dùng destination riêng của người khác; "Kiểm tra kỹ" file của người khác; người quản lý (không phải owner) thêm member → đều phải bị chặn. Test ở Task 7 (`TestIndirectAccessBlocked`).
2. **Session ID / token lộ ra ngoài** — không được xuất hiện trong body JSON trả cho web, trong URL, trong log; DB chỉ chứa hash; invite token chỉ có trong link. Test ở Task 4 (`TestSessionStoredHashed`) và Task 6 (`TestWebLoginNoTokenInBody`).
3. **Setup chạy song song / chạy lại** — hai request setup đồng thời chỉ tạo đúng một admin; setup sau khi đã có user → 409. Test ở Task 1 (`TestCreateFirstAdminOnce`) và Task 4.
4. **Tự khóa admin** — khóa/hạ quyền admin hoạt động cuối cùng bị từ chối; user bị khóa mất mọi session ngay; `fridge recover promote` cứu được. Test ở Task 4 (`TestLastAdminGuard`) và Task 11.
5. **Giả mạo IP qua `X-Forwarded-For`** — request không đến từ proxy tin cậy mà gửi XFF thì XFF bị bỏ qua (không lách được khóa đăng nhập/allowlist log). Test ở Task 6 (`TestClientIPTrustsOnlyProxy`).

---

## File Structure

```
internal/store/store.go                 # + migration 4
internal/store/users.go                 # users, invites, sessions (mới)
internal/store/databases.go             # + OwnerID, OwnerName, members, visible queries
internal/store/destinations.go          # + OwnerID, OwnerName, Shared, visible queries
internal/store/jobs.go                  # + restores.StartedByID, backups-by-dest/key, FailRunning dump_checks
internal/store/checks.go                # dump_checks, image_checks (mới)
internal/scram/scram.go                 # RFC 5802/7677 (mới)
internal/auth/auth.go                   # Service, Principal, errors, setup, credentials
internal/auth/login.go                  # SCRAM start/finish, reauth, rate limit
internal/auth/sessions.go               # session create/authenticate/revoke/purge
internal/auth/users.go                  # invites, accept, users admin, change password
internal/app/access.go                  # helper quyền theo Principal (mới)
internal/app/members.go                 # members DB (mới)
internal/dumpcheck/dumpcheck.go         # header inspect (mới) + testdata/
internal/app/dumpchecks.go              # job "kiểm tra kỹ" (mới)
internal/registry/registry.go           # Docker Hub remote version (mới)
internal/app/images.go                  # trang Images (mới)
internal/container/images.go            # image ops Docker (mới)
internal/api/auth_routes.go             # route xác thực/user/session (mới)
internal/api/middleware.go              # principal, CSRF, client IP, security headers (mới)
internal/api/api.go                     # áp quyền route cũ
internal/api/admin_routes.go            # images, dump checks, members, share (mới)
internal/web/web.go + static/**         # web admin (mới)
cmd/fridge/{main.go,cli.go,login.go,recover.go}
deploy/{docker-compose.yml,traefik.yml,.env.example}, README.md
```

---

### Task 1: Store — migration 4, users, invites, sessions

**Files:**
- Modify: `internal/store/store.go` (thêm migration 4)
- Create: `internal/store/users.go`, `internal/store/users_test.go`

**Interfaces:**
- Produces (migration 4 tạo **toàn bộ** schema giai đoạn 2, các task sau chỉ thêm method):
  - `users(id BIGINT identity PK, username TEXT UNIQUE NOT NULL, role TEXT NOT NULL CHECK (role IN ('admin','user')), scram_salt BYTEA NOT NULL, scram_iterations INT NOT NULL, stored_key BYTEA NOT NULL, server_key BYTEA NOT NULL, disabled BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), created_by BIGINT NULL REFERENCES users(id))`
  - `invites(id identity PK, token_hash BYTEA UNIQUE NOT NULL, username TEXT NOT NULL, role TEXT NOT NULL, purpose TEXT NOT NULL CHECK (purpose IN ('new_user','reset_password')), user_id BIGINT NULL REFERENCES users(id) ON DELETE CASCADE, created_by BIGINT NULL REFERENCES users(id), expires_at TIMESTAMPTZ NOT NULL, used_at TIMESTAMPTZ NULL, created_at …)`
  - `sessions(id_hash BYTEA PRIMARY KEY, user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE, created_at, last_seen_at, expires_at TIMESTAMPTZ NOT NULL, ip TEXT NOT NULL DEFAULT '', user_agent TEXT NOT NULL DEFAULT '')`
  - `ALTER TABLE databases ADD owner_id BIGINT NULL REFERENCES users(id)`; `database_members(database_id BIGINT REFERENCES databases(id) ON DELETE CASCADE, user_id BIGINT REFERENCES users(id) ON DELETE CASCADE, added_by BIGINT NULL REFERENCES users(id), added_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (database_id, user_id))`
  - `ALTER TABLE destinations ADD owner_id BIGINT NULL REFERENCES users(id), ADD shared BOOLEAN NOT NULL DEFAULT false`
  - `ALTER TABLE restores ADD started_by_id BIGINT NULL REFERENCES users(id)`
  - `dump_checks(id identity PK, destination_id BIGINT NOT NULL, s3_key TEXT NOT NULL, status TEXT NOT NULL, result JSONB NOT NULL DEFAULT '{}', log TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', started_by_id BIGINT NULL REFERENCES users(id), started_at, finished_at NULL)`
  - `image_checks(image_ref TEXT PRIMARY KEY, local_digest, local_version, remote_digest, remote_version, error TEXT NOT NULL DEFAULT '', checked_at TIMESTAMPTZ NOT NULL)`

```go
type User struct { ID int64; Username, Role string; Salt []byte; Iterations int; StoredKey, ServerKey []byte; Disabled bool; CreatedAt time.Time; CreatedBy *int64 }
type Invite struct { ID int64; TokenHash []byte; Username, Role, Purpose string; UserID, CreatedBy *int64; ExpiresAt time.Time; UsedAt *time.Time; CreatedAt time.Time }
type Session struct { IDHash []byte; UserID int64; CreatedAt, LastSeenAt, ExpiresAt time.Time; IP, UserAgent string }

func (s *Store) CountUsers(ctx) (int, error)
func (s *Store) CreateFirstAdmin(ctx, u *User) error   // 1 tx + pg_advisory_xact_lock; ErrConflict nếu đã có user; role='admin'; gán owner_id NULL của databases & destinations cho user này
func (s *Store) CreateUser(ctx, u *User) error          // ErrConflict nếu trùng username
func (s *Store) GetUser(ctx, id int64) (User, error)
func (s *Store) GetUserByName(ctx, name string) (User, error)
func (s *Store) ListUsers(ctx) ([]User, error)
func (s *Store) UpdateUserCredentials(ctx, id int64, salt []byte, iter int, stored, server []byte) error
func (s *Store) SetUserRole(ctx, id int64, role string) error
func (s *Store) SetUserDisabled(ctx, id int64, disabled bool) error
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

- [ ] **Step 1: Viết test** (package `store_test`, `storetest.New`)

```go
func TestMigration4Applied(t *testing.T)      // schema_migrations count == 4; cột databases.owner_id, destinations.shared tồn tại (truy vấn information_schema)
func TestCreateFirstAdminOnce(t *testing.T)   // 2 goroutine CreateFirstAdmin đồng thời → đúng 1 nil, 1 ErrConflict; CountUsers==1; DB/destination có sẵn (owner NULL) giờ owner = admin
func TestUserCRUD(t *testing.T)               // CreateUser trùng → ErrConflict; SetUserRole/Disabled; CountActiveAdmins bỏ qua admin disabled
func TestInviteSingleUse(t *testing.T)        // MarkInviteUsed lần 2 → ErrConflict
func TestSessionLifecycle(t *testing.T)       // Create/Get/Touch; DeleteUserSessions(except=h1) giữ h1 xóa h2; DeleteExpiredSessions xóa đúng row quá idle và quá expires_at
func TestFailRunningIncludesDumpChecks(t *testing.T)
```
- [ ] **Step 2:** `bash scripts/test-db.sh && go test ./internal/store/...` → FAIL.
- [ ] **Step 3: Implement.** Cập nhật assertion đếm `schema_migrations` hiện có thành 4.
- [ ] **Step 4:** chạy lại → PASS.
- [ ] **Step 5: Commit** `feat(store): users, invites, sessions and phase-2 schema`.

---

### Task 2: Store — owner, members, destination dùng chung, checks

**Files:**
- Modify: `internal/store/databases.go`, `internal/store/destinations.go`, `internal/store/jobs.go`
- Create: `internal/store/checks.go`, tests tương ứng

**Interfaces:**
- Consumes: migration 4 (Task 1).
- Produces:

```go
// Database thêm: OwnerID *int64; OwnerName string (LEFT JOIN users, chỉ đọc). CreateDatabase ghi owner_id.
func (s *Store) ListDatabasesVisible(ctx, userID int64) ([]Database, error) // owner hoặc member; bỏ status deleted
func (s *Store) IsMember(ctx, dbID, userID int64) (bool, error)
type Member struct { UserID int64; Username string; AddedAt time.Time }
func (s *Store) AddMember(ctx, dbID, userID int64, addedBy *int64) error   // trùng → ErrConflict
func (s *Store) RemoveMember(ctx, dbID, userID int64) error                // không có → ErrNotFound
func (s *Store) ListMembers(ctx, dbID int64) ([]Member, error)
// Destination thêm: OwnerID *int64; OwnerName string; Shared bool. CreateDestination ghi owner_id, shared.
func (s *Store) ListDestinationsVisible(ctx, userID int64) ([]Destination, error) // owner_id=user OR shared
func (s *Store) SetDestinationShared(ctx, id int64, shared bool) error
// Restore thêm: StartedByID *int64 (StartRestore ghi).
func (s *Store) GetBackupByKey(ctx, destID int64, key string) (Backup, error)
func (s *Store) ListOKBackupsOnDestination(ctx, destID int64, visibleTo *int64) ([]Backup, error) // nil = tất cả; ngược lại chỉ DB user thấy được (owner/member); mới nhất trước
type DumpCheck struct { ID, DestinationID int64; S3Key, Status, Log, Error string; Result map[string]any; StartedByID *int64; StartedAt time.Time; FinishedAt *time.Time }
func (s *Store) StartDumpCheck(ctx, c *DumpCheck) error
func (s *Store) FinishDumpCheck(ctx, id int64, status string, result map[string]any, log, errText string) error
func (s *Store) GetDumpCheck(ctx, id int64) (DumpCheck, error)
type ImageCheck struct { ImageRef, LocalDigest, LocalVersion, RemoteDigest, RemoteVersion, Error string; CheckedAt time.Time }
func (s *Store) UpsertImageCheck(ctx, c ImageCheck) error
func (s *Store) ListImageChecks(ctx) ([]ImageCheck, error)
```

- [ ] **Step 1: Viết test**

```go
func TestVisibleDatabases(t *testing.T)    // A owner db1, B owner db2, A member của db2 → A thấy [db1, db2]; B thấy [db2]; OwnerName đúng
func TestMembers(t *testing.T)             // Add trùng → ErrConflict; Remove không có → ErrNotFound; ListMembers có Username
func TestVisibleDestinations(t *testing.T) // A dA (riêng), admin dS (shared=true), admin dP (riêng) → A thấy [dA, dS]
func TestBackupsOnDestination(t *testing.T)// backup ok của db1(A) và db2(B) trên dS → visibleTo=&A chỉ trả db1; nil trả 2; GetBackupByKey tìm đúng, key lạ → ErrNotFound
func TestDumpAndImageChecks(t *testing.T)  // Start/Finish/Get dump check (result JSON round-trip); Upsert image check 2 lần → 1 row, giá trị mới
```
- [ ] **Step 2:** chạy → FAIL. **Step 3:** implement. **Step 4:** PASS.
- [ ] **Step 5: Commit** `feat(store): ownership, members, shared destinations, checks`.

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
type Service struct { Store *store.Store; Secret []byte; SetupToken string; Now func() time.Time /* nil=time.Now */ ; /* RAM state */ }
func New(st *store.Store, secret []byte, setupToken string) *Service
var ErrInvalid, ErrSetupDone, ErrBadSetupToken, ErrBadCredentials, ErrLocked, ErrUnauthenticated, ErrForbidden, ErrLastAdmin, ErrInviteInvalid error

func (s *Service) SetupNeeded(ctx) (bool, error)
func (s *Service) Setup(ctx, setupToken, username string, c Credentials) (store.User, error) // SetupToken rỗng hoặc sai → ErrBadSetupToken; đã có user → ErrSetupDone; dùng Store.CreateFirstAdmin
func (s *Service) LoginStart(ctx, clientFirst, ip string) (serverFirst, scramID string, err error)
func (s *Service) LoginFinish(ctx, scramID, clientFinal, ip, userAgent string) (serverFinal, sessionID string, u store.User, err error)
func (s *Service) ReauthStart(ctx, p Principal, clientFirst string) (serverFirst, scramID string, err error) // username trong clientFirst phải = p.Username
func (s *Service) ReauthFinish(ctx, p Principal, scramID, clientFinal string) (serverFinal, reauthToken string, err error)
func (s *Service) Authenticate(ctx, sessionID string) (Principal, error) // hash → GetSession; quá idle/expires hoặc user disabled → ErrUnauthenticated (và xóa row); Touch nếu last_seen cũ > 1 phút
func (s *Service) Logout(ctx, p Principal) error
func (s *Service) ChangePassword(ctx, p Principal, reauthToken string, c Credentials) error // xóa mọi session khác
func (s *Service) CreateInvite(ctx, p Principal, username, role string) (token string, err error) // chỉ admin; username đã tồn tại → store.ErrConflict
func (s *Service) CreateResetInvite(ctx, createdBy *int64, userID int64) (token string, err error) // dùng cho admin + recover
func (s *Service) InviteInfo(ctx, token string) (username, purpose string, err error)               // không hợp lệ/hết hạn/đã dùng → ErrInviteInvalid
func (s *Service) AcceptInvite(ctx, token string, c Credentials) (store.User, error)                // new_user → CreateUser; reset → UpdateUserCredentials + xóa session; MarkInviteUsed
func (s *Service) ListUsers(ctx, p Principal) ([]store.User, error)              // chỉ admin
func (s *Service) SetRole(ctx, p Principal, userID int64, role string) error     // chỉ admin; hạ admin hoạt động cuối → ErrLastAdmin
func (s *Service) SetDisabled(ctx, p Principal, userID int64, disabled bool) error // chỉ admin; khóa admin hoạt động cuối → ErrLastAdmin; khóa → DeleteUserSessions
func (s *Service) Promote(ctx, username string) error                            // không cần principal (recover)
func (s *Service) ListSessions(ctx, p Principal, all bool) ([]store.Session, error) // all chỉ admin
func (s *Service) RevokeSession(ctx, p Principal, idHash []byte) error              // của mình hoặc admin; không thấy → store.ErrNotFound
func (s *Service) PurgeExpired(ctx) (int, error)
func SessionHash(sessionID string) []byte
```
- Hằng số đúng Global Constraints (iterations, TTL, rate limit…), khai báo `const` có tên; `now` chỉnh được trong test.
- User không tồn tại / bị khóa ở `LoginStart`: salt giả `HMAC-SHA256(Secret, "fridge-fake-salt:"+username)[:16]`, iterations 600000, vẫn trả `serverFirst`; ở `LoginFinish` chạy `Finish` với khóa ngẫu nhiên rồi trả `ErrBadCredentials` (cùng lỗi như sai mật khẩu). Rate limit đếm lần sai theo `username+"|"+ip`; khi bị khóa → `ErrLocked` ngay ở `LoginStart`.
- Token (session, invite, reauth): 32 byte `crypto/rand`, base64url không padding; chỉ hash vào DB/RAM.

- [ ] **Step 1: Viết test** (store thật qua `storetest`, client dùng `scram.NewClient`, iterations 600000 thật — chấp nhận chậm ~1s/test; helper `creds(password)` sinh Credentials)

```go
func TestSetup(t *testing.T)               // SetupToken "" → ErrBadSetupToken; sai token → ErrBadSetupToken; đúng → admin; lần 2 → ErrSetupDone; SetupNeeded false
func TestLoginSuccessAndSession(t *testing.T) // start/finish → client.Verify(serverFinal)==nil; Authenticate(sessionID) trả Principal đúng role
func TestSessionStoredHashed(t *testing.T) // row sessions.id_hash == sha256(sessionID), không có cột nào chứa sessionID thô
func TestLoginWrongPasswordGeneric(t *testing.T) // sai mật khẩu và user không tồn tại đều ErrBadCredentials; serverFirst của user không tồn tại có dạng hợp lệ (s=, i=600000) và ổn định giữa 2 lần
func TestLoginRateLimit(t *testing.T)      // 5 lần sai cùng (user, ip) → LoginStart lần 6 ErrLocked; IP khác vẫn thử được; Now +5m → mở khóa
func TestScramStateExpires(t *testing.T)   // Now +61s giữa start và finish → ErrBadCredentials
func TestSessionExpiry(t *testing.T)       // Now +12h01m không hoạt động → ErrUnauthenticated; hoạt động mỗi giờ nhưng quá 7 ngày → ErrUnauthenticated
func TestLogoutAndChangePassword(t *testing.T) // Logout → ErrUnauthenticated; ChangePassword không reauth token / token hết hạn → ErrForbidden; có token → mật khẩu mới đăng nhập được, session khác bị xóa, session hiện tại còn
func TestInviteFlow(t *testing.T)          // user thường CreateInvite → ErrForbidden; admin tạo → InviteInfo đúng; Accept → đăng nhập được; Accept lần 2 → ErrInviteInvalid; Now +49h → ErrInviteInvalid
func TestResetInvite(t *testing.T)         // reset → mật khẩu cũ hỏng, session cũ bị xóa
func TestLastAdminGuard(t *testing.T)      // 1 admin: SetRole(user)/SetDisabled(true) chính nó → ErrLastAdmin; thêm admin thứ 2 rồi khóa admin 1 → OK và session của admin 1 → ErrUnauthenticated; Promote("bob") → role admin
func TestRevokeSession(t *testing.T)       // user không revoke được session người khác (ErrNotFound); admin được
```
- [ ] **Step 2:** `bash scripts/test-db.sh && go test ./internal/auth/` → FAIL. **Step 3:** implement. **Step 4:** PASS + `go test -race ./internal/auth/`.
- [ ] **Step 5: Commit** `feat(auth): setup, SCRAM login, stateful sessions, invites, users`.

---

### Task 5: App — owner, members, helper quyền, duyệt file restore

**Files:**
- Create: `internal/app/access.go`, `internal/app/members.go`, tests
- Modify: `internal/app/databases.go`, `destinations.go`, `restore.go`, `fakes_test.go`

**Interfaces:**
- Consumes: store Task 2, `auth.Principal`.
- Produces:

```go
// CreateRequest thêm OwnerID int64 (0 = không owner, chỉ dùng cho job hệ thống); RestoreRequest thêm OwnerID int64 → node mới & restores.started_by_id.
func (a *App) VisibleDatabases(ctx, p auth.Principal) ([]store.Database, error) // admin: ListDatabases; user: ListDatabasesVisible
func (a *App) CanAccessDB(ctx, p auth.Principal, d store.Database) (bool, error) // admin || owner || member
func IsDBOwner(p auth.Principal, d store.Database) bool                          // admin || owner
func (a *App) VisibleDestinations(ctx, p auth.Principal) ([]store.Destination, error)
func CanManageDestination(p auth.Principal, d store.Destination) bool // admin || owner
func CanUseDestination(p auth.Principal, d store.Destination) bool    // manage || shared
func (a *App) SetDestinationShared(ctx, id int64, shared bool) error
func (a *App) AddMember(ctx, d store.Database, username string, addedBy int64) error // username không có → ErrInvalid; owner tự thêm mình → ErrInvalid
func (a *App) RemoveMember(ctx, d store.Database, userID int64) error
func (a *App) ListMembers(ctx, d store.Database) ([]store.Member, error)
type Restorable struct { Key string; Size int64; LastModified time.Time; DatabaseName string /* rỗng nếu file lạ */ }
func (a *App) ListRestorable(ctx, p auth.Principal, destID int64, prefix, q string) ([]Restorable, bool, error)
	// dest không thấy → store.ErrNotFound; CanManage → liệt kê bucket (ListObjects); shared không manage → ListOKBackupsOnDestination(visibleTo=&p.UserID) lọc theo prefix/q
func (a *App) CanRestoreKey(ctx, p auth.Principal, destID int64, key string) (bool, error)
	// CanManage(dest) || (GetBackupByKey(dest,key) thuộc DB mà CanAccessDB)
```
- `defaultSchedule` chỉ chọn destination **owner của DB dùng được** (owner sở hữu hoặc shared, id nhỏ nhất); không có → không tạo lịch.

- [ ] **Step 1: Viết test** (fake runtime/objects sẵn có; users tạo bằng store)

```go
func TestOwnershipOnCreate(t *testing.T)   // CreateDatabase OwnerID=A → d.OwnerID==A; restore OwnerID=B → node mới owner B, restores.started_by_id==B
func TestAccessHelpers(t *testing.T)       // bảng: admin/owner/member/khác × CanAccessDB, IsDBOwner; CanManage/CanUse với dest riêng/shared
func TestMembersApp(t *testing.T)          // AddMember "nobody" → ErrInvalid; thêm B → B thấy DB qua VisibleDatabases; Remove → mất
func TestListRestorableShared(t *testing.T) // dS shared (admin); backup db1(A) + db2(B) trên dS → A: chỉ key db1, có DatabaseName; admin: liệt kê bucket (fakeObjects có thêm "upload/x.sql.gz")
func TestListRestorablePrivateHidden(t *testing.T) // dest riêng của B → A nhận store.ErrNotFound
func TestCanRestoreKey(t *testing.T)       // A với key backup db2(B) trên dS → false; key db1 → true; B trên dest của mình với file lạ → true
func TestDefaultScheduleUsesOwnersDestination(t *testing.T) // dest riêng của admin không được dùng cho DB của A; có dS shared → dùng dS
```
- [ ] **Step 2:** FAIL. **Step 3:** implement; cập nhật call site/tests cũ (`CreateRequest`/`RestoreRequest` thêm field). **Step 4:** `go test ./internal/app/ && go test -race ./internal/app/` PASS.
- [ ] **Step 5: Commit** `feat(app): resource ownership, members and access helpers`.

---

### Task 6: API — xác thực, route auth/user/session, header bảo mật

**Files:**
- Create: `internal/api/middleware.go`, `internal/api/auth_routes.go`, tests
- Modify: `internal/api/api.go` (chữ ký `New`, bỏ token tĩnh, `/api/ca` công khai), `cmd/fridge/main.go` (wiring tạm để build)

**Interfaces:**
- Consumes: `auth.Service` (Task 4).
- Produces:

```go
type Options struct {
	Web          http.Handler       // nil → 404 cho đường dẫn ngoài /api, /healthz, /readyz
	TrustedProxy func(ip net.IP) bool // nil → không tin XFF
}
func New(a *app.App, au *auth.Service, o Options) http.Handler
func principal(r *http.Request) (auth.Principal, bool) // lấy từ context
func clientIP(r *http.Request, trusted func(net.IP) bool) string // RemoteAddr; nếu peer tin cậy → phần tử **cuối** của X-Forwarded-For
func TrustHosts(hosts ...string) func(net.IP) bool // resolve lúc khởi tạo (net.LookupIP) + loopback
```
- Route **công khai**: `GET /healthz`, `GET /readyz`, `GET /api/ca`, `GET /api/setup/status` → `{"needed":bool}`, `POST /api/setup` `{setup_token, username, credentials}`, `POST /api/auth/scram/start` `{client_first}` → `{server_first, scram_id}`, `POST /api/auth/scram/finish` `{scram_id, client_final, want_token?}` → `{server_final, user:{id,username,role}, session_id?}` + Set-Cookie, `GET /api/invites/{token}` → `{username, purpose}`, `POST /api/invites/{token}/accept` `{credentials}`.
- Route **cần đăng nhập**: `POST /api/auth/logout`, `GET /api/me`, `POST /api/me/reauth/start|finish` (finish → `{server_final, reauth_token}`), `POST /api/me/password` `{reauth_token, credentials}`, `GET /api/me/sessions`, `DELETE /api/sessions/{id}` (`id` = hex của id_hash). **Admin**: `GET /api/users`, `POST /api/invites` `{username, role}` → `{link}`, `POST /api/users/{id}/reset` → `{link}`, `PATCH /api/users/{id}` `{role?, disabled?}`, `GET /api/sessions`.
- `credentials` JSON: `{salt, iterations, stored_key, server_key}` (base64 chuẩn).
- `session_id` chỉ có trong body khi request gửi `"want_token": true` (CLI). Cookie luôn được set.
- Link mời: `https://<r.Host>/#/invite/<token>`.
- Middleware: principal từ cookie `fridge_session` hoặc `Authorization: Bearer <session>`; request dùng **cookie** + method không phải GET/HEAD/OPTIONS phải có `Origin` khớp `https://`+`r.Host` (hoặc `Sec-Fetch-Site: same-origin`) → không thì 403. Chưa setup + không principal → 401 `setup required`. Header mọi response: `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`.
- Map lỗi: `ErrBadCredentials/ErrUnauthenticated` 401, `ErrBadSetupToken/ErrForbidden` 403, `ErrSetupDone/ErrLastAdmin/ErrConflict` 409, `ErrLocked` 429, `ErrInvalid/ErrInviteInvalid` 400.
- Route cũ giữ nguyên handler nhưng đặt sau middleware (áp quyền ở Task 7). Tạm thời: user đã đăng nhập dùng được mọi route cũ.

- [ ] **Step 1: Viết test** (httptest + store thật + `scram.NewClient`; helper `loginAs(t, srv, user, pw) (cookie, sessionID)`)

```go
func TestSetupRequired(t *testing.T)       // chưa setup: GET /api/databases → 401 {"error":"setup required"}; /api/setup/status → needed true; /api/ca → 200 PEM
func TestSetupAndLoginWeb(t *testing.T)    // setup → scram start/finish → Set-Cookie fridge_session có HttpOnly, Secure, SameSite=Strict; GET /api/me bằng cookie → username
func TestWebLoginNoTokenInBody(t *testing.T) // finish không want_token → body không có "session_id"; có want_token → có và Bearer dùng được
func TestCSRF(t *testing.T)                // POST bằng cookie, Origin "https://evil" → 403; Origin đúng → qua; Bearer không Origin → qua
func TestClientIPTrustsOnlyProxy(t *testing.T) // clientIP: peer 10.9.9.9 không tin + XFF "1.2.3.4" → "10.9.9.9"; peer tin cậy + XFF "1.1.1.1, 5.6.7.8" → "5.6.7.8"
func TestAdminOnlyRoutes(t *testing.T)     // user thường GET /api/users → 403; admin POST /api/invites → link chứa "/#/invite/"
func TestSecurityHeaders(t *testing.T)     // mọi response có CSP "default-src 'self'"
func TestLogoutInvalidatesBearer(t *testing.T)
```
- [ ] **Step 2:** FAIL. **Step 3:** implement; `main.go` truyền `auth.New(store, secretKeyBytes, env FRIDGE_SETUP_TOKEN)` và `Options{TrustedProxy: TrustHosts(env FRIDGE_TRUSTED_PROXY default "traefik")}` (lookup lỗi → log, chỉ tin loopback); bỏ yêu cầu `FRIDGE_TOKEN`; scheduler gọi `au.PurgeExpired` mỗi giờ. Sửa test API cũ dùng đăng nhập thay token.
- [ ] **Step 4:** `go test ./internal/api/ ./cmd/fridge/` PASS.
- [ ] **Step 5: Commit** `feat(api): session auth, SCRAM routes, users and invites`.

---

### Task 7: API — áp quyền trên mọi route tài nguyên

**Files:** Modify `internal/api/api.go`; Create `internal/api/admin_routes.go` (members, share), `internal/api/authz_test.go`

**Interfaces:**
- Consumes: helper Task 5, principal Task 6.
- Produces:
  - `listDBs` → `VisibleDatabases`; `dbOf` → 404 nếu `!CanAccessDB`; `createDB` → `OwnerID = p.UserID`. `dbJSON` thêm `owner`.
  - Members: `GET /api/databases/{db}/members`, `POST /api/databases/{db}/members` `{username}`, `DELETE /api/databases/{db}/members/{user_id}` — POST/DELETE chỉ `IsDBOwner` (người quản lý → 403).
  - Destinations: list → `VisibleDestinations`; update/delete/test → 404 nếu không thấy, 403 nếu thấy (shared) nhưng `!CanManage`; create → `OwnerID = p.UserID`, `shared=false`; `PATCH /api/destinations/{id}` `{shared}` chỉ admin. `destJSON` thêm `owner`, `shared`; với destination shared mà người xem không manage: ẩn `endpoint`, `access_key`, `bucket`.
  - `GET /api/destinations/{id}/objects` → `ListRestorable` (trả `{objects:[{key,size,last_modified,database}], truncated}`).
  - Schedules create/update: cần DB access + `CanUseDestination(dest)` (không → 400 "destination not usable"). Backup now có `destination_id` → `CanUse`.
  - `getBackup`/`download`: backup thuộc DB có quyền (không → 404).
  - `startRestore`: `CanRestoreKey` (false → 404); `OwnerID = p.UserID`. `getRestore`: admin hoặc `started_by_id == p.UserID` (không → 404).
  - `/api/settings/allowlist` GET/PUT: chỉ admin (403).

- [ ] **Step 1: Viết test** (3 user: admin, A, B; fake runtime/objects)

```go
func TestDBVisibility(t *testing.T)        // A tạo a1; B GET /api/databases không có a1; GET /api/databases/a1 → 404; admin thấy, owner="a"
func TestMembersAPI(t *testing.T)          // A thêm B → B thấy a1, connection_string có; B POST members C → 403; B DELETE chính mình → 403
func TestDestinationOwnership(t *testing.T)// A tạo dA; B list không có, PUT/DELETE/test → 404; admin PATCH shared dAdmin → B thấy (không có endpoint/bucket/access_key), PUT → 403; user PATCH shared → 403
func TestScheduleDestinationRules(t *testing.T) // B đặt lịch cho DB của mình bằng dA (riêng A) → 400; bằng shared → 201
func TestIndirectAccessBlocked(t *testing.T) {
	// trên dS shared có backup a1 (A): B POST /api/restores với key đó → 404; B GET /api/backups/<id a1>/download → 404
	// B GET /api/destinations/dS/objects không chứa key của a1; B POST /api/databases/a1/backups → 404
}
func TestRestoreOwnership(t *testing.T)    // A restore key a1 → 202; node mới owner A; B GET /api/restores/<id> → 404
func TestAllowlistAdminOnly(t *testing.T)  // A GET/PUT → 403; admin OK
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/api/` PASS.
- [ ] **Step 5: Commit** `feat(api): enforce per-resource permissions`.

---

### Task 8: Validate dump — mức 1 (header) và mức 2 ("Kiểm tra kỹ")

**Files:**
- Create: `internal/dumpcheck/dumpcheck.go`, `dumpcheck_test.go`, `internal/dumpcheck/testdata/` (dump thật: `pg14.dump`, `pg17.dump` sinh bằng `pg_dump -Fc` từ container `postgres:14`/`postgres:17` chạy pg_dump cùng major, `pg13-by-pgdump17.dump` = server 13 dump bằng pg_dump 17, `plain.sql.gz`, `not-a-dump.dump` (một PNG nhỏ), `empty.dump`)
- Create: `internal/app/dumpchecks.go` + test; Modify: `internal/app/app.go` (ObjectStore thêm `ReadHead`), `internal/objstore/objstore.go`, `internal/app/restore.go`, `internal/container/docker.go` (RunSpec thêm `Cmd []string`, `NoNetwork bool`), `internal/api/admin_routes.go`, fakes

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

// app
func (a *App) StartDumpCheck(ctx, destID int64, key string, startedBy int64) (store.DumpCheck, error)
func (a *App) GetDumpCheck(ctx, id int64) (store.DumpCheck, error)
```
- `StartRestore` (phần đồng bộ, **trước** khi tạo row/node): `ReadHead` + `Inspect`; lỗi → `ErrInvalid` với lý do. Với `.dump`: `ServerVersion` ghi đè version nguồn cho `CheckCompat`; `Major(PgDumpVersion) > Major(target version)` → `ErrInvalid` "dump made by pg_dump X cannot be restored by pg_restore Y".
- Mức 2 (nền, `a.wg`, timeout 6h): `.dump` → container tạm `fridge-check-<id>` image `postgres:<version cao nhất catalog>`, `Cmd ["sleep","infinity"]`, `NoNetwork`, rồi `Exec pg_restore -v -f /dev/null` với stdin là object; exit ≠ 0 → failed + stderr; đếm dòng stderr chứa `processing item` → `toc_entries` (best-effort); luôn `Remove` container (ctx detached). `.sql.gz`/`.sql` → kiểm trong Go (gunzip hết + `utf8` từng khối, xử lý rune vắt qua ranh giới). `result`: `{format, bytes, source_version, pg_dump_version, toc_entries}`.
- API: `POST /api/dump-checks` `{destination_id, key}` → 202 (quyền: `CanRestoreKey`, không → 404); `GET /api/dump-checks/{id}` (admin hoặc `started_by_id`).

- [ ] **Step 1: Tạo testdata** bằng script trong WSL (container postgres tạm, bảng mẫu vài dòng; commit file nhị phân, mỗi file < 50 KB). **Viết test:**

```go
func TestInspectCustom(t *testing.T)   // pg14.dump → Format custom, Major(ServerVersion)==14, Major(PgDumpVersion)==14; pg17.dump → 17
func TestInspectPgDumpNewer(t *testing.T) // pg13-by-pgdump17.dump → server 13, pg_dump 17
func TestInspectRejects(t *testing.T)  // empty.dump → "empty"; not-a-dump.dump → "not a pg_dump custom-format"; "x.zip" → "unsupported"; "x.sql" với byte NUL → lỗi
func TestInspectSQL(t *testing.T)      // plain.sql.gz (64KB đầu) → Format sql, Gzip true; head bị cắt giữa rune UTF-8 vẫn OK
// app
func TestRestoreHeaderChecks(t *testing.T) // fakeObjects chứa các file testdata: not-a-dump → ErrInvalid, không có Run, không có restore row; pg17.dump → node v16 → ErrInvalid; pg13-by-pgdump17.dump → node v16 → ErrInvalid (pg_dump); pg14.dump → node 17 OK
func TestDumpCheckSQLGzTruncated(t *testing.T) // .sql.gz bị cắt nửa → check failed, error chứa "unexpected EOF"; nguyên vẹn → ok, bytes đúng
func TestDumpCheckDumpUsesTempContainer(t *testing.T) // fakeRuntime: Run với NoNetwork, Cmd sleep; Exec pg_restore nhận stdin; exit 1 → failed; Remove luôn được gọi
```
```go
// integration (//go:build integration, internal/app): TestDumpCheckRealPgRestore — pg14.dump nguyên → ok, toc_entries > 0; cắt đôi → failed; container fridge-check-* không còn
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./internal/dumpcheck/ ./internal/app/ ./internal/api/` + integration test PASS.
- [ ] **Step 5: Commit** `feat: validate dump files before restore (header + deep check)`.

---

### Task 9: Trang Images — dữ liệu & API

**Files:**
- Create: `internal/container/images.go`, `internal/registry/registry.go` + test, `internal/app/images.go` + test
- Modify: `internal/app/app.go` (interface `Images` riêng), `internal/app/scheduler.go`, `internal/api/admin_routes.go`, `cmd/fridge/main.go`

**Interfaces:**
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
func (a *App) PullImages(refs []string) (jobID string, err error) // ref phải thuộc catalog → ErrInvalid; job RAM
func (a *App) PullStatus(jobID string) (map[string]string, error) // ref → "pulling|done|error: …"
func (a *App) PrunePreview(ctx) ([]ImageRow, int64, error)    // Present && len(UsedBy)==0
func (a *App) Prune(ctx, refs []string) ([]string, error)     // kiểm lại ngay trước khi xóa; ref đang dùng/không trong preview → bỏ qua; trả các ref đã xóa
func (a *App) CheckImageUpdates(ctx) error                    // mỗi image catalog có mặt: RemoteDigest ≠ local RepoDigest → RemoteEnv(PG_VERSION) → UpsertImageCheck; lỗi mạng → ghi Error, không trả lỗi tổng
```
- `App` thêm field `Img Images` (interface gồm các method container ở trên) và `Registry interface{ RemoteEnv(...) }`; nil trong test cũ.
- Scheduler thêm entry cố định `"17 4 * * *"` gọi `CheckImageUpdates`.
- API (admin, user → 403): `GET /api/images`, `POST /api/images/pull` `{refs}` → `{job}`, `GET /api/images/pull/{job}`, `GET /api/images/prune` (preview `{images, bytes}`), `POST /api/images/prune` `{refs}`, `POST /api/images/check-updates`.

- [ ] **Step 1: Viết test**

```go
func TestRegistryRemoteEnv(t *testing.T)  // httptest giả auth + registry: token → manifest list có amd64/arm64 → config env ["PG_VERSION=18.2"] → "18.2"; 401 không token → lỗi
func TestListImagesUsage(t *testing.T)    // fake Images: postgres:16 có mặt, 2 container DB dùng (1 dừng) → UsedBy 2 tên, LastUsed = StartedAt mới nhất; postgres:15 chưa có → Present false
func TestPrune(t *testing.T)              // preview chỉ image không dùng; Prune với ref đang dùng → bỏ qua, không gọi ImageRemove; ref tag lạ ngoài catalog → bỏ qua
func TestPullValidation(t *testing.T)     // "nginx:latest" → ErrInvalid; "postgres:15" → job, status done
func TestCheckImageUpdates(t *testing.T)  // local digest khác remote → ImageCheck RemoteVersion "18.2", LocalVersion "18.1"; registry lỗi → Error ghi lại, hàm trả nil
func TestImagesAdminOnly(t *testing.T)    // api: user → 403
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** PASS (+ `go test -tags integration ./internal/container/` thêm case ImageInspect/DBContainers thật).
- [ ] **Step 5: Commit** `feat: images management (pull, prune, usage, update check)`.

---

### Task 10: HTTPS + deploy

**Files:**
- Modify: `internal/pki/ca.go` (+ test), `internal/proxy/traefik.go` (+ test), `internal/app/app.go` (`WebCert, WebKey string`; `SetAllowlist` truyền), `cmd/fridge/main.go`, `deploy/traefik.yml`, `deploy/docker-compose.yml`, `README.md`
- Create: `deploy/.env.example`, `cmd/fridge/webcert.go` (+ test)

**Interfaces:**
```go
func (c *CA) IssueNames(names []string, validFor time.Duration) (certPEM, keyPEM []byte, err error) // tên là IP → IPAddresses, còn lại → DNSNames; Issue(domain) gọi IssueNames([]string{domain})
func ensureWebCert(ca *pki.CA, certFile, keyFile string, hosts []string) error // SAN = localhost, 127.0.0.1 + hosts; sinh lại khi SAN khác hoặc còn < 30 ngày; hạn 365 ngày
func WriteBase(dir string, cidrs []string, apiBackend, webCert, webKey string) error
```
- `WriteBase` tạo: router HTTP `fridge-web` (entryPoints `[websecure]`, `PathPrefix(`/`)`, `tls: {}`, middleware `allowlist-http@file`, service `fridge-api`); router `fridge-redirect` (entryPoints `[web]`, `PathPrefix(`/`)`, middleware `to-https@file`, service `fridge-api`) với middleware `to-https: redirectScheme {scheme: https, port: "8443", permanent: true}`; `tls.stores.default.defaultCertificate {certFile: webCert, keyFile: webKey}`. Giữ middleware TCP.
- Cert web ở `<FRIDGE_TRAEFIK_DIR>/certs/web.crt|web.key`; `main` gọi `ensureWebCert` trước `WriteBase` (env `FRIDGE_PUBLIC_HOSTS` tách dấu phẩy, trim).
- `traefik.yml`: entryPoints `web: ":8080"`, `websecure: ":8443"`, `pg: ":5432"`, `ping: ":8082"`.
- Compose: traefik publish `8080`, `8443`, `5432`; fridge env bỏ `FRIDGE_TOKEN`, thêm `FRIDGE_SETUP_TOKEN: ${FRIDGE_SETUP_TOKEN:-}`, `FRIDGE_PUBLIC_HOSTS: ${FRIDGE_PUBLIC_HOSTS:-}`, `FRIDGE_TRUSTED_PROXY: traefik`.
- `deploy/.env.example`: như bản trước nhưng `FRIDGE_TOKEN` → `FRIDGE_SETUP_TOKEN` (giải thích: chỉ dùng lần tạo admin đầu tiên, `openssl rand -hex 32`), thêm `FRIDGE_PUBLIC_HOSTS=` (ví dụ `fridge.lan,192.168.1.10`). **Không chứa secret thật.**
- README: truy cập `https://localhost:8443`, setup lần đầu, cài CA, `fridge login`, các lệnh `recover`.

- [ ] **Step 1: Viết test**

```go
func TestIssueNames(t *testing.T)          // ["localhost","127.0.0.1","fridge.lan"] → DNSNames [localhost fridge.lan], IPAddresses [127.0.0.1]; verify với CA
func TestEnsureWebCert(t *testing.T)       // lần 1 tạo; lần 2 cùng hosts → không đổi file (mtime/nội dung); hosts khác → sinh lại
func TestWriteBaseHTTPS(t *testing.T)      // yaml: fridge-web entryPoints [websecure] có tls; fridge-redirect dùng to-https; redirectScheme port "8443"; tls.stores.default.defaultCertificate.certFile == webCert
```
- [ ] **Step 2:** FAIL. **Step 3:** implement + file deploy + README. **Step 4:** `go test ./...` PASS; `docker compose -f deploy/docker-compose.yml --env-file <file tạm đủ biến> config -q` OK.
- [ ] **Step 5: Commit** `feat(deploy): HTTPS on :8443 with self-signed web cert`.

---

### Task 11: CLI — login, CA TOFU, check, members, share, recover

**Files:** Modify `cmd/fridge/cli.go`, `cmd/fridge/main.go`; Create `cmd/fridge/login.go`, `cmd/fridge/recover.go` + tests

**Interfaces:**
- Consumes: API Task 6–9, `scram` Task 3, `auth` Task 4, `proxy`/`store` (recover).
- Produces lệnh:
  - `fridge login [--user U] [--password-stdin] [--yes] [--ca-file F]`: lần đầu chưa có CA → GET `/api/ca` (chỉ request này bỏ qua kiểm TLS), in `SHA256 fingerprint: AA:BB:…`, hỏi `Trust this CA? [y/N]` (`--yes` bỏ qua hỏi), lưu `~/.config/fridge/ca.pem`; SCRAM qua `/api/auth/scram/*` với `want_token:true`, `Verify` server signature; lưu `~/.config/fridge/session` (JSON `{url, session}`, 0600). Nhập mật khẩu không echo bằng `stty -echo` (lỗi → cảnh báo, đọc có echo).
  - `fridge logout`, `fridge whoami`.
  - `fridge setup --setup-token T --user U [--password-stdin]` (cho server không có trình duyệt; đăng ký Credentials tính bằng `scram`). *(Lựa chọn của plan)*
  - `fridge check --from <dest>:<key> [--wait]`, `fridge members ls|add|rm <db> [user]`, `fridge dest share <name|id> on|off`.
  - Mọi request: `FRIDGE_URL` mặc định `https://localhost:8443`; token = env `FRIDGE_TOKEN` nếu có, ngược lại từ file session; TLS tin CA từ `--ca-file`/`FRIDGE_CA_FILE`/`ca.pem` đã lưu + system pool; **không** có cờ insecure. 401 → in `not logged in — run: fridge login`; `setup required` → in hướng dẫn mở web hoặc `fridge setup`.
  - `fridge recover allowlist <cidr>... | reset-password <user> | promote <user>`: chạy **trong container**, đọc env server (`FRIDGE_DATABASE_URL`, `FRIDGE_SECRET_KEY`, `FRIDGE_TRAEFIK_DIR`, `FRIDGE_API_BACKEND`, `FRIDGE_PUBLIC_HOSTS`), thao tác thẳng store/proxy (allowlist → `SetSetting` + `WriteBase`; reset → `auth.CreateResetInvite(nil, id)` in `https://<host đầu tiên hoặc localhost>:8443/#/invite/<token>`; promote → `auth.Promote`).

- [ ] **Step 1: Viết test**

```go
func TestLoginTOFU(t *testing.T)        // httptest TLS server ký bằng CA test: --yes → ca.pem lưu, fingerprint in ra; lần 2 không hỏi; server đổi CA khác → lỗi TLS (không tự tin lại)
func TestLoginSCRAMSavesSession(t *testing.T) // fake API scram (dùng scram.StartServer) → file session 0600 chứa session; whoami gửi Bearer
func TestTokenEnvOverrides(t *testing.T)
func TestNotLoggedInMessage(t *testing.T) // 401 → stderr "fridge login", exit 1
func TestCheckAndMembersCommands(t *testing.T) // body đúng route/field
func TestRecoverAllowlist(t *testing.T)  // store thật + thư mục traefik tạm: recover allowlist 10.0.0.0/8 → setting cập nhật, _base.yml có CIDR
func TestRecoverResetAndPromote(t *testing.T) // in link chứa "/#/invite/"; promote → role admin
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** `go test ./cmd/fridge/` + `go test -race ./cmd/fridge/` PASS.
- [ ] **Step 5: Commit** `feat(cli): session login with CA TOFU, check, members, recover`.

---

### Task 12: Web — khung, SCRAM JS, setup/đăng nhập/lời mời/đổi mật khẩu

**Files:**
- Create: `internal/web/web.go` + test, `internal/web/static/index.html`, `static/css/app.css`, `static/js/{app.js,api.js,scram.js,router.js,layout.js,ui.js}`, `static/js/pages/{setup.js,login.js,invite.js,password.js}`, `static/test/scram.html`, `static/js/test-scram.js`, `static/vendor/bootstrap/…` (5.3.3: `bootstrap.min.css`, `bootstrap.bundle.min.js`), `static/vendor/bootstrap-icons/…` (1.11.3: css + fonts)
- Modify: `cmd/fridge/main.go` (truyền `Options.Web = web.Handler()`)

**Interfaces:**
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
// layout.js: sidebar trái đóng/mở (nút ☰), thu gọn chỉ còn icon, nhớ localStorage "fridge.sidebar"; mục Databases · Restore · Destinations · Settings; admin thêm Users · Sessions · Images (dựa vào /api/me); dưới: username · Đổi mật khẩu · Đăng xuất
```
- Không inline `<script>`/`style=`/`onclick` (CSP). Ngôn ngữ giao diện: tiếng Việt.
- Trang setup: setup token, username, mật khẩu ×2 (≥ 12 ký tự, trùng nhau) → `makeCredentials` → `POST /api/setup` → tự đăng nhập. Login: username/mật khẩu → `scramLogin`. Invite: hiện username/purpose từ `GET /api/invites/{token}`, đặt mật khẩu. Đổi mật khẩu: mật khẩu cũ (reauth) + mới ×2.
- `/test/scram.html`: chạy vector RFC 7677 (user/pencil, salt `W22ZaJ0SNY7soEsUEjb6gQ==`, i=4096, nonces như Task 3) và in `PASS`/`FAIL` vào `<pre id="result">`.

- [ ] **Step 1: Viết test Go**

```go
func TestHandlerServesIndexAndVendor(t *testing.T) // "/" → 200 text/html; "/vendor/bootstrap/bootstrap.min.css" → 200 text/css; "/nope.js" → 404
func TestNoInlineScripts(t *testing.T)  // quét mọi .html trong embed: không có "<script>" có nội dung inline, không có thuộc tính on*= hay style=
```
- [ ] **Step 2:** FAIL. **Step 3:** implement (tải Bootstrap/Icons đúng phiên bản từ jsDelivr vào repo một lần; ghi phiên bản trong `static/vendor/VERSIONS.txt`).
- [ ] **Step 4: Kiểm tra trình duyệt thật bằng Playwright trong Docker** (không phải dependency của dự án, không ảnh hưởng frontend):
  - Tạo `scripts/ui-smoke/smoke.mjs` + `scripts/ui-smoke/run.sh`; `run.sh` chạy `docker run --rm --network host -v "$PWD/scripts/ui-smoke:/work" -w /work mcr.microsoft.com/playwright:v1.49.0-noble sh -c 'npm i --no-save playwright@1.49.0 >/dev/null && node smoke.mjs'` với env `BASE=https://localhost:8443`, `SETUP_TOKEN`, `ADMIN_PW`. Browser mở với `ignoreHTTPSErrors: true`.
  - `smoke.mjs` (phần của Task 12): mở `/test/scram.html` → `#result` chứa `PASS`; mở `/` trên DB trống → thấy form setup; điền setup token + admin → vào Databases; đăng xuất → đăng nhập lại; **gom mọi lỗi console/`securitypolicyviolation`** → có lỗi thì exit 1. In từng bước PASS/FAIL.
  - Chạy trên stack compose riêng (project `fridge-ui`, volume mới); ghi output vào report; dọn project.
- [ ] **Step 5:** `go test ./internal/web/ ./cmd/fridge/` PASS. **Commit** `feat(web): admin shell, SCRAM in browser, setup/login/invite/password`.

---

### Task 13: Web — Databases, chi tiết DB, Restore, Destinations

**Files:** Create `internal/web/static/js/pages/{databases.js,database.js,restore.js,destinations.js}`, cập nhật `router.js`, `app.css`

**Interfaces:** chỉ dùng API Task 6–9 qua `api.js`.
- **Databases**: bảng (tên, engine/version, domain, trạng thái màu, backup gần nhất, cột owner nếu admin), nút "+ Tạo DB" (modal: engine, version dropdown từ `/api/engines`, tên, domain) → sau tạo chuyển sang chi tiết.
- **Chi tiết DB** (mockup đã duyệt): header + nút **Backup now** (poll trạng thái backup 2–3s); tab **Kết nối** (connection string ẩn mật khẩu, nút Hiện/Copy, host/user/db, link tải CA `/api/ca`); tab **Backups** (bảng: thời gian, nguồn lịch/tay, destination, dung lượng, trạng thái, Tải · Restore… · Log trong modal); tab **Lịch backup** (CRUD, destination dropdown = destination có thể dùng); tab **Người quản lý** (chỉ owner/admin sửa; người khác chỉ xem).
- **Restore**: chọn destination → bảng file (`/objects`, ô tìm kiếm, cột DB) → chọn file → nút **Kiểm tra kỹ** (job, hiện kết quả) và form node mới (tên, domain, version — gợi ý version nguồn) → **Restore** → trang trạng thái polling; lỗi mức 1 hiện ngay dưới form. "Restore…" từ tab Backups mở trang này với file chọn sẵn.
- **Destinations**: bảng của mình (admin: tất cả + owner + công tắc dùng chung); thêm/sửa (secret để trống = giữ)/xóa (xác nhận)/Test.

- [ ] **Step 1:** Viết trang.
- [ ] **Step 2: Mở rộng `scripts/ui-smoke/smoke.mjs`** (Task 12) với các bước, chạy trên stack compose `fridge-ui` (có MinIO dev):
  - admin tạo destination MinIO → Test OK; tạo DB → hiện trong bảng, trạng thái running; Hiện connection string thấy mật khẩu; Backup now → dòng backup `ok`; Restore… → Kiểm tra kỹ → `ok`; restore vào node mới → `ok`.
  - admin mời user B (lấy link từ UI) → mở link trong context trình duyệt mới, đặt mật khẩu, đăng nhập → B không thấy DB/destination của admin; admin bật **dùng chung** → B thấy destination đó trong dropdown Lịch backup nhưng không có nút Sửa.
  - Không có lỗi console/CSP ở bất kỳ trang nào.
- [ ] **Step 3:** `go test ./...` vẫn PASS (`TestNoInlineScripts`). **Commit** `feat(web): databases, restore and destinations pages`.

---

### Task 14: Web — Settings, Users, Sessions, Images

**Files:** Create `internal/web/static/js/pages/{settings.js,users.js,sessions.js,images.js}`, cập nhật `router.js`, `layout.js` (chấm báo Images khi có bản mới)

- **Settings**: allowlist (admin; textarea CIDR, lưu), tải CA cert, phiên của tôi (bảng: tạo lúc, hoạt động cuối, IP, UA, nút thu hồi; đánh dấu phiên hiện tại).
- **Users** (admin): bảng user (username, role, trạng thái, tạo lúc), nút **Mời** (username, role → hiện link để copy), Khóa/Mở, Nâng/Hạ admin (lỗi admin cuối hiện rõ), **Reset mật khẩu** (hiện link).
- **Sessions** (admin): mọi phiên, thu hồi.
- **Images** (admin): bảng (tag, digest rút gọn, dung lượng, ngày tạo, version, số DB đang dùng + danh sách tên, lần dùng cuối, huy hiệu **"New version: 18.1 → 18.2"**); nút Pull (chọn engine+version) / Pull tất cả (poll job), **Kiểm tra cập nhật**, **Prune** (modal xác nhận liệt kê image + tổng dung lượng → xóa). Sidebar mục Images có chấm khi có ít nhất một bản mới.

- [ ] **Step 1:** Viết trang.
- [ ] **Step 2: Mở rộng `smoke.mjs`**: khóa user B → trang của B (context riêng) bị đưa về đăng nhập ở request kế tiếp; hạ quyền admin duy nhất → thông báo lỗi hiển thị; thu hồi một phiên trong Settings → phiên đó mất; Images: Pull `postgres:15` → hiện "đã có"; Prune preview không chứa image đang được DB dùng; Kiểm tra cập nhật → mỗi dòng có trạng thái (hoặc "không kiểm tra được"); không lỗi console/CSP.
- [ ] **Step 3:** `go test ./...` PASS. **Commit** `feat(web): settings, users, sessions and images pages`.

---

### Task 15: E2E + smoke giai đoạn 2

**Files:** Create `internal/api/e2e_auth_integration_test.go` (`//go:build integration`); Modify `README.md` nếu smoke phát hiện sai lệch

**Interfaces:** Consumes tất cả.

- [ ] **Step 1: Viết integration test** (store thật + Docker thật + MinIO `fridge-it-minio`, httptest server của `api.New`, client SCRAM của `scram`):

```go
func TestE2EOwnershipFlow(t *testing.T) {
	// setup admin → admin mời A, B → A, B accept + login (Bearer)
	// A tạo destination riêng (MinIO) + DB postgres 16 → Backup now → ok
	// B: không thấy DB/destination của A (404); A thêm B làm member → B backup được, B không thêm member (403)
	// admin tạo dS shared; B đặt lịch bằng dS (201), bằng dest của A (400)
	// A restore bản backup vào node mới v17 → owner A; B không xem restore của A (404)
	// file rác trên dest của A → POST /api/restores 400 trước khi tạo container (đếm container fridge-postgres-* không đổi)
	// dọn: removeNode các node, xóa object
}
```
- [ ] **Step 2:** Chạy `go test -tags integration ./internal/api/ -run E2E -v` → PASS.
- [ ] **Step 3: Smoke compose** (WSL, project riêng `fridge-smoke`, volume mới): `up -d --build` với `FRIDGE_SETUP_TOKEN` → `curl -kI http://localhost:8080` = 301/308 tới `https://localhost:8443`; `openssl s_client -connect localhost:8443` cert có SAN `localhost`; `fridge login` (TOFU) trước setup → báo setup required; `fridge setup` → `fridge login` → `fridge ls`; restart fridge → `POST /api/setup` 409; `docker exec … /fridge recover allowlist 203.0.113.0/24` rồi `… 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16` → truy cập bị chặn rồi mở lại; `recover reset-password admin` in link; `bash scripts/ui-smoke/run.sh` (toàn bộ smoke UI) → PASS. Ghi lệnh + kết quả vào report. Dọn chỉ project của mình + container `fridge-postgres-*` đã tạo.
- [ ] **Step 4:** `go build ./... && go vet ./... && go test ./...` + `go test -race ./internal/... ./cmd/...` PASS.
- [ ] **Step 5: Commit** `test: phase-2 end-to-end ownership flow and smoke`.
