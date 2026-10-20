# Fridge Giai đoạn 1 — Xương sống Postgres — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Tạo node Postgres (chọn version) cho một domain, truy cập qua Traefik theo SNI có IP allowlist và cert internal CA, backup lên nhiều S3 destination theo lịch hoặc 1 click, restore 1 file S3 vào node mới — qua REST API + CLI.

**Architecture:** 1 binary Go `fridge`. Lá: `secret` (AES-GCM), `store` (Postgres metadata qua pgx), `engine` (catalog + Postgres), `container` (Docker SDK), `pki` (internal CA), `proxy` (ghi file dynamic Traefik), `objstore` (minio-go), `alert` (webhook). Lõi `app` điều phối, phụ thuộc Docker và S3 qua 2 interface nhỏ để test bằng fake. `api` (net/http) và CLI (`cmd/fridge`) nằm ngoài cùng. Data plane (Traefik + container DB) chạy độc lập với Fridge.

**Tech Stack:** Go 1.26, Postgres 17 (metadata) + `github.com/jackc/pgx/v5`, Docker Engine Go SDK, `github.com/minio/minio-go/v7`, `github.com/robfig/cron/v3`, `gopkg.in/yaml.v3`, Traefik v3, stdlib `net/http` + `flag`.

**Spec:** `docs/superpowers/specs/2026-10-20-fridge-design.md` (mục "Giai đoạn triển khai" bước 1).

## Global Constraints

- **Mọi lệnh build/test/docker chạy trong WSL** (người dùng yêu cầu): distro `Ubuntu-24.04`, repo tại `/mnt/d/Windows/Desktop/code/Fridge`. Từ Windows gọi: `wsl -d Ubuntu-24.04 -- bash -lc 'cd /mnt/d/Windows/Desktop/code/Fridge && <lệnh>'`. Docker Engine chạy native trong WSL (29.8.2, `/var/run/docker.sock`), không dùng Docker Desktop. Git cũng chạy trong WSL.
- Go trong WSL là 1.22.2 (apt). Không cài thêm: `go.mod` khai `go 1.26` để Go tự tải toolchain 1.26 (`GOTOOLCHAIN=auto`, mặc định).
- Module path `fridge`. Go ≥ 1.26.
- **Metadata DB = Postgres 17** (người dùng đổi từ SQLite): container `fridge-meta` trong compose, trên `fridge-net`, không publish port, không do Fridge quản lý/route. Fridge kết nối qua env `FRIDGE_DATABASE_URL`.
- **Test cần Postgres thật**: env `FRIDGE_TEST_DATABASE_URL`, mặc định `postgres://postgres:postgres@localhost:55432/postgres?sslmode=disable`; `scripts/test-db.sh` dựng container `fridge-test-pg` (`postgres:17`, port `55432:5432`, idempotent). Không kết nối được → test **fail** kèm hướng dẫn chạy script, không được skip im lặng. Chỉ được thêm 5 dependency ở Tech Stack (+ dependency gián tiếp của chúng). CLI dùng stdlib `flag`.
- Engine giai đoạn 1: chỉ `postgres`. Catalog Postgres: `13, 14, 15, 16, 17, 18`, mặc định `17`. Version ngoài catalog → 400.
- Mỗi DB = 1 container `fridge-postgres-<name>` + named volume `fridge-postgres-<name>`, trên network `fridge-net`, **không publish port**, restart `unless-stopped`.
- Traefik terminate TLS; entrypoint `pg` = `:5432`; router `HostSNI(\`<domain>\`)`; backend nói plaintext trong `fridge-net`.
- IP allowlist toàn cục, mặc định đúng 3 dải `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, áp cho mọi router TCP và HTTP API.
- `tls_mode` giai đoạn 1 chỉ nhận `internal` (mặc định). `acme` → 400 `"acme lands in phase 3"`.
- Mật khẩu DB và S3 secret key lưu mã hóa AES-GCM; khóa = env `FRIDGE_SECRET_KEY` (base64, đúng 32 byte). Không trả về secret của destination qua API; mật khẩu DB chỉ xuất hiện trong `connection_string`.
- Key S3 của backup: `<prefix>/<engine>-<version>/<db>/<db>-<YYYYMMDD-HHMMSS>.<ext>` (giờ UTC), metadata `fridge-engine`, `fridge-version`, `fridge-db`. Prefix mặc định `fridge`.
- Schedule mặc định khi tạo DB (nếu đã có ít nhất 1 destination): destination có id nhỏ nhất, cron `"<fnv32a(name)%60> 2 * * *"`, prefix `fridge`, keep 7, enabled.
- Backup/restore stream thẳng giữa `docker exec` và S3, không ghi đệm ra đĩa.
- Auth giai đoạn 1: 1 bearer token tĩnh `FRIDGE_TOKEN` (user/role ở giai đoạn 3). *(Quyết định của plan)*
- Ngoài phạm vi giai đoạn 1 (đừng làm): MySQL/MariaDB/Mongo/Redis, ProxySQL, ACME, user/role/audit, xóa DB, restore ghi đè, test-restore, reconcile, gia hạn cert, Web UI, SSE log.

## Review Focus

1. **Fridge restart giữa lúc backup/restore** → các dòng `running` phải chuyển `failed` ("interrupted by restart") khi khởi động, nếu không DB bị khóa backup vĩnh viễn. Test ở Task 3 (`FailRunning`) + gọi ở Task 13.
2. **`pg_dump` chết giữa chừng khi upload đang chạy** → backup phải `failed`, upload bị hủy (không có object được tham chiếu), alert được gửi. Test ở Task 9.
3. **Bấm "Backup now" khi lịch đang chạy cùng DB** → lần thứ hai nhận 409, không chạy song song. Ràng buộc DB ở Task 3, test ở Task 9.
4. **Tạo DB thất bại sau khi container đã chạy** (không healthy / timeout) → rollback hết container, volume, route, cert; row `error`; tạo lại cùng tên được. Test ở Task 8.
5. **Restore file không phải của Fridge hoặc file hỏng** → `.sql.gz` upload tay restore được khi truyền engine; file rác → restore `failed` với log lỗi, node mới bị gỡ (row `error`), không để lại container mồ côi. Test ở Task 10.

---

## File Structure

```
go.mod
cmd/fridge/main.go          # dispatch: server | CLI subcommands; server wiring
cmd/fridge/cli.go           # HTTP client helper + CLI subcommands
internal/secret/secret.go   # AES-GCM Box
internal/store/store.go     # Open, migrations, errors, types
internal/store/databases.go
internal/store/destinations.go
internal/store/settings.go
internal/store/jobs.go      # schedules, backups, restores, FailRunning
internal/store/storetest/storetest.go  # New(t): DB Postgres tạm riêng cho mỗi test
scripts/test-db.sh          # dựng Postgres cho test
internal/engine/engine.go   # interface, Info, catalog, ResolveVersion, CompareVersions
internal/engine/postgres.go
internal/container/docker.go
internal/pki/ca.go
internal/proxy/traefik.go
internal/objstore/objstore.go
internal/alert/alert.go
internal/app/app.go         # App, interfaces, errors, allowlist
internal/app/databases.go   # CreateDatabase, removeNode, ConnString
internal/app/destinations.go
internal/app/backup.go      # StartBackup, runBackup, retention, schedules CRUD
internal/app/scheduler.go
internal/app/restore.go     # DetectSource, CheckCompat, StartRestore
internal/app/fakes_test.go  # fakeRuntime, fakeObjects
internal/api/api.go
deploy/Dockerfile
deploy/docker-compose.yml
deploy/traefik.yml
```

Test file nằm cạnh file nguồn (`*_test.go`). Test cần Docker/MinIO thật có build tag `integration`.

---

### Task 1: Module + secret.Box

**Files:**
- Create: `go.mod`, `.gitattributes`, `internal/secret/secret.go`, `internal/secret/secret_test.go`

**Interfaces:**
- Produces: `secret.NewBox(keyB64 string) (*Box, error)`, `(*Box).Encrypt(plain string) (string, error)`, `(*Box).Decrypt(enc string) (string, error)`.

- [ ] **Step 1: Chuẩn bị repo trong WSL**
  - `.gitattributes` = `* text=auto eol=lf` (repo được `git init` từ Windows; ép LF để script/YAML chạy được trong Linux). Chạy `git add --renormalize .`.
  - `go mod init fridge && go mod edit -go=1.26`
  - `go version` (trong thư mục repo) → in `go1.26.x` (toolchain tự tải xong).
- [ ] **Step 2: Viết test**

```go
func TestBoxRoundTrip(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	b, err := NewBox(key); if err != nil { t.Fatal(err) }
	enc, _ := b.Encrypt("s3cr3t")
	enc2, _ := b.Encrypt("s3cr3t")
	if enc == "s3cr3t" || enc == enc2 { t.Fatal("must be encrypted with random nonce") }
	got, err := b.Decrypt(enc)
	if err != nil || got != "s3cr3t" { t.Fatalf("got %q %v", got, err) }
}
func TestBoxRejectsBadKey(t *testing.T) {
	if _, err := NewBox(base64.StdEncoding.EncodeToString(make([]byte, 16))); err == nil { t.Fatal("16-byte key accepted") }
	if _, err := NewBox("not base64!"); err == nil { t.Fatal("bad base64 accepted") }
}
func TestBoxWrongKeyFails(t *testing.T) {
	a, _ := NewBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	b, _ := NewBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))
	enc, _ := a.Encrypt("x")
	if _, err := b.Decrypt(enc); err == nil { t.Fatal("decrypt with wrong key succeeded") }
}
```

- [ ] **Step 3: Chạy `go test ./internal/secret/`** → FAIL (undefined).
- [ ] **Step 4: Implement** — `crypto/aes` + `cipher.NewGCM`; output = base64(nonce‖ciphertext).
- [ ] **Step 5: Chạy lại** → PASS.
- [ ] **Step 6: Commit** `feat: add secret box (AES-GCM)`.

---

### Task 2: Store — mở DB, migrations, databases, destinations, settings

**Files:**
- Create: `internal/store/store.go`, `databases.go`, `destinations.go`, `settings.go`, `store_test.go` (package `store_test`), `internal/store/storetest/storetest.go`, `scripts/test-db.sh`

**Interfaces:**
- Produces:

```go
var ErrNotFound, ErrConflict, ErrInUse error
type Database struct { ID int64; Name, Engine, Version, Domain, TLSMode, User, PasswordEnc, HostID, ContainerID, Volume, Status string; CreatedAt time.Time }
type Destination struct { ID int64; Name, Provider, Endpoint, Region, Bucket, AccessKey, SecretKeyEnc string; PathStyle bool; CreatedAt time.Time }
func Open(ctx context.Context, url string) (*Store, error) // pgxpool.New + Ping; chạy migrations
func (s *Store) Close() error
func (s *Store) Ping(ctx) error
func (s *Store) CreateDatabase(ctx, d *Database) error            // set ID, CreatedAt; HostID mặc định "local"
func (s *Store) GetDatabase(ctx, id int64) (Database, error)
func (s *Store) GetDatabaseByName(ctx, name string) (Database, error) // bỏ qua row status error/deleted
func (s *Store) ListDatabases(ctx) ([]Database, error)            // mọi status trừ deleted, theo id
func (s *Store) SetDatabaseState(ctx, id int64, status, containerID string) error
func (s *Store) CreateDestination(ctx, d *Destination) error
func (s *Store) GetDestination(ctx, id int64) (Destination, error)
func (s *Store) ListDestinations(ctx) ([]Destination, error)      // theo id tăng dần
func (s *Store) UpdateDestination(ctx, d Destination) error
func (s *Store) DeleteDestination(ctx, id int64) error            // ErrInUse nếu còn schedule tham chiếu
func (s *Store) GetSetting(ctx, key string) (string, bool, error)
func (s *Store) SetSetting(ctx, key, value string) error
```

```go
// storetest
func New(t *testing.T) *store.Store // đọc FRIDGE_TEST_DATABASE_URL (mặc định ở Global Constraints); CREATE DATABASE fridge_t_<8 hex ngẫu nhiên>; store.Open trên DB đó; t.Cleanup: Close + DROP DATABASE ... WITH (FORCE). Lỗi kết nối → t.Fatalf("test postgres not reachable at %s — run scripts/test-db.sh: %v")
func URL(t *testing.T) string        // URL của DB tạm vừa tạo bởi New gần nhất trong test (dùng khi cần mở lại)
```
`storetest` import `store`, nên test trong thư mục `store` dùng package `store_test` để tránh vòng import.

Migrations: slice `[]string`, bảng `schema_migrations(version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`; mỗi migration chạy trong 1 transaction có `SELECT pg_advisory_xact_lock(hashtext('fridge-migrate'))` (an toàn khi sau này chạy nhiều instance). Kiểu cột: id `BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY`, thời gian `TIMESTAMPTZ NOT NULL DEFAULT now()`, cờ `BOOLEAN`. Migration 1 tạo **toàn bộ** bảng của giai đoạn 1 (cả `backup_schedules`, `backups`, `restores` dùng ở Task 3) theo mô hình ở spec mục 1, với:
- `CREATE UNIQUE INDEX db_name ON databases(name) WHERE status NOT IN ('error','deleted')`
- `CREATE UNIQUE INDEX db_domain ON databases(engine, domain) WHERE status NOT IN ('error','deleted')`
- `destinations.name UNIQUE`; `backup_schedules.destination_id REFERENCES destinations(id) ON DELETE RESTRICT`, `database_id REFERENCES databases(id)`
- `CREATE UNIQUE INDEX one_running_backup ON backups(database_id) WHERE status='running'`
- `backups.destination_id`, `restores.destination_id` là INTEGER thường (không FK) để giữ lịch sử khi xóa destination. *(Quyết định của plan)*
- Bỏ bảng `users`, `api_tokens`, `audit_log`, `hosts` (giai đoạn 3/sau); `databases.host_id TEXT NOT NULL DEFAULT 'local'`.

Map lỗi qua `*pgconn.PgError`: code `23505` (unique) → `ErrConflict`; `23503` (FK) → `ErrInUse`; `pgx.ErrNoRows` → `ErrNotFound`.

- [ ] **Step 1: Viết `scripts/test-db.sh`** (`docker start fridge-test-pg 2>/dev/null || docker run -d --name fridge-test-pg -p 55432:5432 -e POSTGRES_PASSWORD=postgres postgres:17`, rồi chờ `pg_isready` qua `docker exec`), chạy nó → `docker exec fridge-test-pg pg_isready` báo `accepting connections`.
- [ ] **Step 2: Viết test** (dùng `storetest.New(t)`)

```go
func TestReopenKeepsDataAndSkipsMigrations(t *testing.T)   // tạo db, Close, store.Open(ctx, storetest.URL(t)) lại → GetDatabaseByName thấy; migrations không chạy lại (schema_migrations vẫn 1 dòng)
func TestDatabaseUniqueness(t *testing.T) {
	// abc/postgres/abc.com OK; trùng name → errors.Is(err, ErrConflict); khác name cùng engine+domain → ErrConflict
	// SetDatabaseState(abc,"error","") rồi tạo lại name abc, domain abc.com → OK
}
func TestDestinationDeleteInUse(t *testing.T) // dest + db + schedule tham chiếu → DeleteDestination = ErrInUse; xóa schedule → OK; Get sau xóa = ErrNotFound
func TestSettings(t *testing.T)               // Get chưa có → ok=false; Set "a"; Set "b" (upsert) → "b", true
```

- [ ] **Step 3: Chạy `go test ./internal/store/...`** → FAIL.
- [ ] **Step 4: Implement** (`pgxpool`). Schedule trong `TestDestinationDeleteInUse` chèn bằng SQL thô nếu Task 3 chưa có.
- [ ] **Step 5: Chạy lại** → PASS.
- [ ] **Step 6: Commit** `feat(store): postgres metadata store with databases, destinations, settings`.

---

### Task 3: Store — schedules, backups, restores

**Files:**
- Create: `internal/store/jobs.go`, `internal/store/jobs_test.go`

**Interfaces:**
- Produces:

```go
type Schedule struct { ID, DatabaseID, DestinationID int64; Cron, Prefix string; KeepLatest int; Enabled bool }
type Backup struct { ID, DatabaseID, DestinationID int64; ScheduleID *int64; Trigger, Engine, EngineVersion, Status, S3Key, Log, Error string; SizeBytes int64; StartedAt time.Time; FinishedAt *time.Time }
type Restore struct { ID, DestinationID int64; S3Key, SourceEngine, SourceVersion, Mode, Status, Log, Error string; TargetDatabaseID *int64; StartedAt time.Time; FinishedAt *time.Time }

func (s *Store) CreateSchedule(ctx, sc *Schedule) error
func (s *Store) GetSchedule(ctx, id int64) (Schedule, error)
func (s *Store) ListSchedules(ctx, databaseID int64) ([]Schedule, error)
func (s *Store) ListEnabledSchedules(ctx) ([]Schedule, error)
func (s *Store) UpdateSchedule(ctx, sc Schedule) error
func (s *Store) DeleteSchedule(ctx, id int64) error

func (s *Store) StartBackup(ctx, b *Backup) error   // status=running, StartedAt=now; ErrConflict nếu DB đã có backup running
func (s *Store) FinishBackup(ctx, id int64, status string, size int64, log, errText string) error
func (s *Store) GetBackup(ctx, id int64) (Backup, error)
func (s *Store) ListBackups(ctx, databaseID int64) ([]Backup, error)            // mới nhất trước
func (s *Store) OKBackupsForSchedule(ctx, scheduleID int64) ([]Backup, error)   // status ok, mới nhất trước
func (s *Store) DeleteBackup(ctx, id int64) error

func (s *Store) StartRestore(ctx, r *Restore) error  // status=running
func (s *Store) SetRestoreTarget(ctx, id, databaseID int64) error
func (s *Store) FinishRestore(ctx, id int64, status, log, errText string) error
func (s *Store) GetRestore(ctx, id int64) (Restore, error)

func (s *Store) FailRunning(ctx, reason string) (int, error) // mọi backups+restores running → failed, error=reason, finished_at=now; trả tổng số dòng
```

- [ ] **Step 1: Viết test**

```go
func TestOneRunningBackupPerDatabase(t *testing.T) // StartBackup lần 2 cùng DB → ErrConflict; FinishBackup(ok) lần 1 → StartBackup lại OK; DB khác chạy song song OK
func TestOKBackupsForScheduleOrder(t *testing.T)   // 3 backup ok + 1 failed cùng schedule → trả 3, ID giảm dần; backup manual (ScheduleID nil) không có mặt
func TestFailRunning(t *testing.T)                 // 1 backup + 1 restore running, 1 backup ok → FailRunning("interrupted by restart") = 2; cả hai status failed, Error đúng reason, FinishedAt != nil; backup ok giữ nguyên
func TestListEnabledSchedules(t *testing.T)        // 2 schedule, 1 disabled → trả 1
```

- [ ] **Step 2: `go test ./internal/store/`** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Chạy lại** → PASS.
- [ ] **Step 5: Commit** `feat(store): schedules, backups, restores`.

---

### Task 4: Engine catalog + Postgres

**Files:**
- Create: `internal/engine/engine.go`, `internal/engine/postgres.go`, `internal/engine/engine_test.go`

**Interfaces:**
- Produces:

```go
type Params struct { Name, User, Password, Version string }
type Spec struct { Image string; Env []string; VolumePath string; Port int; Healthcheck []string }
type Engine interface {
	Spec(p Params) Spec
	DumpCmd(p Params) []string
	DumpExt() string
	RestoreCmd(p Params, key string) (cmd []string, gunzip bool, err error)
	ConnString(p Params, domain string) string
}
type Info struct { Name string; Versions []string; Default string; EntryPoint string; PublicPort int }
func Get(name string) (Engine, Info, error)          // lỗi nếu engine chưa đăng ký
func List() []Info
func ResolveVersion(name, version string) (string, error) // "" → Default; ngoài Versions → lỗi
func CompareVersions(a, b string) int                // so từng thành phần số trên phần chung: ("16","16.4")=0, ("7.2","7.4")<0, ("18","9")>0
```

Postgres: `Info{Name:"postgres", Versions:13..18, Default:"17", EntryPoint:"pg", PublicPort:5432}`.
- `Spec`: Image `postgres:<v>`; Env `POSTGRES_USER=<User>`, `POSTGRES_PASSWORD=<Password>`, `POSTGRES_DB=<Name>`; VolumePath `/var/lib/postgresql` nếu major ≥ 18, ngược lại `/var/lib/postgresql/data` (image 18 đổi PGDATA); Port 5432; Healthcheck `["CMD-SHELL","pg_isready -U <User> -d <Name>"]`.
- `DumpCmd`: `pg_dump -U <User> -d <Name> -Fc`; `DumpExt` = `.dump`.
- `RestoreCmd` theo đuôi key: `.dump` → `pg_restore -U u -d n --no-owner --no-acl --exit-on-error`; `.sql` → `psql -U u -d n -v ON_ERROR_STOP=1 -q`; `.sql.gz` → như `.sql`, gunzip=true; khác → lỗi.
- `ConnString`: `postgresql://<User>:<url.QueryEscape(Password)>@<domain>:5432/<Name>?sslmode=require`.
- Lệnh exec trong container không cần mật khẩu (socket local của image postgres là trust).

- [ ] **Step 1: Viết test**

```go
func TestResolveVersion(t *testing.T) // ("postgres","")="17"; ("postgres","14")="14"; ("postgres","99") lỗi; ("mysql","8.4") lỗi (chưa đăng ký)
func TestCompareVersions(t *testing.T) // các ví dụ ở signature
func TestPostgresVolumePath(t *testing.T) // v17 → "/var/lib/postgresql/data"; v18 → "/var/lib/postgresql"
func TestPostgresRestoreCmd(t *testing.T) // "a/b.dump" → cmd[0]=="pg_restore", gunzip=false; "x.sql.gz" → "psql", true; "x.zip" → err
func TestPostgresConnString(t *testing.T) // Password "p@ss/w" → chứa "p%40ss%2Fw@abc.com:5432/abc?sslmode=require"
```

- [ ] **Step 2: `go test ./internal/engine/`** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Chạy lại** → PASS.
- [ ] **Step 5: Commit** `feat(engine): version catalog and postgres engine`.

---

### Task 5: Docker runtime

**Files:**
- Create: `internal/container/docker.go`, `internal/container/docker_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Produces:

```go
type RunSpec struct { Name, Image string; Env []string; Volume, VolumePath string; Healthcheck []string; Labels map[string]string }
func New(network string) (*Client, error)                  // client.FromEnv + WithAPIVersionNegotiation
func (c *Client) EnsureNetwork(ctx) error                   // tạo bridge network nếu chưa có
func (c *Client) Ping(ctx) error
func (c *Client) Run(ctx, s RunSpec) (id string, err error) // pull image nếu thiếu (đọc hết stream pull), tạo volume, create+start; restart unless-stopped; gắn network với alias = Name; không publish port
func (c *Client) WaitHealthy(ctx, id string, timeout time.Duration) error // poll inspect 1s; "unhealthy" hoặc hết giờ → lỗi
func (c *Client) Exec(ctx, id string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int, err error)
func (c *Client) Remove(ctx, id, volume string) error       // force-remove container rồi volume; not-found = nil; id hoặc volume rỗng thì bỏ qua phần đó
```

`Exec`: ExecCreate (AttachStdout/Stderr, AttachStdin nếu stdin != nil) → ExecAttach (hijacked) → goroutine copy stdin rồi `CloseWrite()` → `stdcopy.StdCopy(stdout, stderr, resp.Reader)` → ExecInspect lấy exit code. Dùng module Docker SDK mà `go get` resolve được hiện tại (`github.com/docker/docker` hoặc `github.com/moby/moby/client`), pin trong go.mod.

- [ ] **Step 1: Viết integration test** (cần Docker daemon trong WSL: `docker info` chạy được)

```go
func TestRunExecRemove(t *testing.T) {
	// network "fridge-test-net"; Run postgres:17-alpine name "fridge-it-pg" volume "fridge-it-pg", env POSTGRES_PASSWORD=x, healthcheck pg_isready
	// WaitHealthy(60s) nil
	// Exec psql -U postgres -tAc "select 41+1" → exit 0, stdout trim == "42"
	// Exec với stdin "select 1;" vào psql -U postgres -tA → stdout "1"
	// Exec "sh -c 'exit 3'" → exitCode 3, err nil
	// Remove → ContainerInspect not found; Remove lần 2 → nil
}
```

- [ ] **Step 2: `go test -tags integration ./internal/container/`** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Chạy lại** → PASS. `go vet ./...` sạch.
- [ ] **Step 5: Commit** `feat(container): docker runtime wrapper`.

---

### Task 6: Internal CA

**Files:**
- Create: `internal/pki/ca.go`, `internal/pki/ca_test.go`

**Interfaces:**
- Produces: `pki.LoadOrCreate(dir string) (*CA, error)` (file `ca.crt`, `ca.key` mode 0600; ECDSA P-256; hiệu lực 10 năm; CN `Fridge Internal CA`), `(*CA).CertPEM() []byte`, `(*CA).Issue(domain string, validFor time.Duration) (certPEM, keyPEM []byte, err error)` (SAN DNS = domain, ExtKeyUsage ServerAuth).

- [ ] **Step 1: Viết test**

```go
func TestIssueVerifiesAgainstCA(t *testing.T) // Issue("abc.com", 365*24h); x509.Verify với pool chứa CA, DNSName "abc.com" → OK; DNSName "xyz.com" → lỗi
func TestLoadOrCreateIsStable(t *testing.T)   // 2 lần LoadOrCreate cùng dir → CertPEM bằng nhau; ca.key mode 0600 (dùng t.TempDir() — nằm trên ext4 của WSL nên mode thật, khác /mnt/d)
```

- [ ] **Step 2: `go test ./internal/pki/`** → FAIL.
- [ ] **Step 3: Implement** bằng stdlib `crypto/x509`, `crypto/ecdsa`.
- [ ] **Step 4: Chạy lại** → PASS.
- [ ] **Step 5: Commit** `feat(pki): internal CA`.

---

### Task 7: Traefik config writer

**Files:**
- Create: `internal/proxy/traefik.go`, `internal/proxy/traefik_test.go`

**Interfaces:**
- Produces:

```go
type Route struct { ID int64; EntryPoint, Domain, Backend, CertFile, KeyFile string } // Backend "host:port"
func WriteRoute(dir string, r Route) error     // <dir>/dynamic/db-<id>.yml
func RemoveRoute(dir string, id int64) error   // not-exist = nil
func WriteCert(dir string, id int64, certPEM, keyPEM []byte) (certFile, keyFile string, err error) // <dir>/certs/db-<id>.crt|.key (key 0600); trả đường dẫn tuyệt đối
func RemoveCert(dir string, id int64) error
func WriteBase(dir string, cidrs []string, apiBackend string) error // <dir>/dynamic/_base.yml
```

Mọi file ghi atomic: ghi `*.tmp` cùng thư mục → `os.Rename`. Dùng `yaml.v3` marshal map, không ghép chuỗi. Fridge và Traefik mount cùng volume ở **cùng đường dẫn** (`/traefik`), nên `certFile` ghi trong yaml là đường dẫn Fridge thấy. Traefik chỉ watch `<dir>/dynamic`.

Nội dung route (router và service đều tên `db-<id>`):
```yaml
tcp:
  routers:
    db-7: { entryPoints: [pg], rule: "HostSNI(`abc.com`)", service: db-7, middlewares: [allowlist-tcp@file], tls: {} }
  services:
    db-7: { loadBalancer: { servers: [ { address: "fridge-postgres-abc:5432" } ] } }
tls:
  certificates: [ { certFile: /traefik/certs/db-7.crt, keyFile: /traefik/certs/db-7.key } ]
```
Nội dung base: `tcp.middlewares.allowlist-tcp.ipAllowList.sourceRange`, `http.middlewares.allowlist-http.ipAllowList.sourceRange` (cùng danh sách CIDR), `http.routers.fridge-api {entryPoints:[api], rule:"PathPrefix(`/`)", service: fridge-api, middlewares:[allowlist-http@file]}`, `http.services.fridge-api.loadBalancer.servers[0].url = apiBackend`.

- [ ] **Step 1: Viết test** (parse yaml ngược lại thành `map[string]any` rồi assert)

```go
func TestWriteRoute(t *testing.T)  // rule == "HostSNI(`abc.com`)"; entryPoints == [pg]; middlewares == [allowlist-tcp@file]; address == "fridge-postgres-abc:5432"; certFile khớp; thư mục dynamic không còn *.tmp
func TestRemoveRouteIdempotent(t *testing.T)
func TestWriteBase(t *testing.T)   // sourceRange tcp và http == 3 CIDR truyền vào; servers[0].url == "http://fridge:8080"
```

- [ ] **Step 2: `go test ./internal/proxy/`** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Chạy lại** → PASS.
- [ ] **Step 5: Commit** `feat(proxy): traefik dynamic config writer`.

---

### Task 8: app — App, CreateDatabase, allowlist

**Files:**
- Create: `internal/app/app.go`, `internal/app/databases.go`, `internal/app/fakes_test.go`, `internal/app/databases_test.go`

**Interfaces:**
- Consumes: store (Task 2–3), engine (4), `container.RunSpec` (5), pki (6), proxy (7), secret (1).
- Produces:

```go
var ErrInvalid = errors.New("invalid") // bọc: fmt.Errorf("%w: domain must be a hostname", ErrInvalid)
type Runtime interface {
	Ping(ctx) error
	Run(ctx, s container.RunSpec) (string, error)
	WaitHealthy(ctx, id string, timeout time.Duration) error
	Exec(ctx, id string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	Remove(ctx, id, volume string) error
}
type App struct {
	Store *store.Store; Box *secret.Box; RT Runtime; CA *pki.CA
	TraefikDir string; HealthTimeout time.Duration
	Alert func(ctx context.Context, text string)
	Sched *Scheduler // có thể nil trong test
	wg sync.WaitGroup
}
func (a *App) Wait()                                      // chờ mọi job nền
type CreateRequest struct { Engine, Name, Domain, Version, TLSMode string }
func (a *App) CreateDatabase(ctx, req CreateRequest) (store.Database, error)
func (a *App) ConnString(d store.Database) (string, error)
func (a *App) removeNode(ctx, d store.Database)           // Remove container+volume, RemoveRoute, RemoveCert, status "error"; lỗi chỉ log
func (a *App) Allowlist(ctx) ([]string, error)            // setting "allowlist" (JSON array) hoặc 3 dải mặc định
func (a *App) SetAllowlist(ctx, cidrs []string) error     // mỗi CIDR qua netip.ParsePrefix (lỗi → ErrInvalid), rỗng → ErrInvalid; lưu; proxy.WriteBase
```

Luồng `CreateDatabase`:
1. Chuẩn hóa: `Domain` lowercase + trim; `TLSMode` rỗng → `internal`, `acme` → ErrInvalid "acme lands in phase 3", khác → ErrInvalid.
2. Validate: name `^[a-z][a-z0-9-]{1,30}$`; domain là hostname (nhãn `[a-z0-9-]`, có ít nhất 1 dấu chấm, ≤ 253); `engine.Get` + `engine.ResolveVersion` (lỗi → ErrInvalid).
3. `User = name + "_app"`, password = `crypto/rand.Text()`, mã hóa bằng Box; `Volume = ContainerName = "fridge-<engine>-<name>"`; `Store.CreateDatabase` status `creating` (ErrConflict trả nguyên).
4. `RT.Run` (label `fridge.db_id`) → `WaitHealthy(HealthTimeout)` → `CA.Issue(domain, 365*24h)` → `WriteCert` → `WriteRoute` (Backend `<container>:<Spec.Port>`, EntryPoint từ Info) → status `running` + containerID.
5. Lỗi ở bước 4 → `removeNode`, trả lỗi.
6. Thành công + có destination → tạo schedule mặc định (Global Constraints), gọi `Sched.Reload` nếu `Sched != nil`.

- [ ] **Step 1: Viết fakes** trong `fakes_test.go`: `fakeRuntime` (ghi lại các lần Run/Remove; field `healthErr error`; `execFn func(cmd []string, stdin io.Reader, stdout, stderr io.Writer) int`) và `newTestApp(t) (*App, *fakeRuntime)` dùng `storetest.New(t)`, CA tạm, TraefikDir tạm, Alert ghi vào slice.
- [ ] **Step 2: Viết test**

```go
func TestCreateDatabaseHappyPath(t *testing.T) {
	// Domain "ABC.com " → d.Domain=="abc.com", Version=="17", Status=="running", User=="abc_app", PasswordEnc không chứa password thật
	// fakeRuntime.runs[0].Name=="fridge-postgres-abc", Image=="postgres:17"
	// file <TraefikDir>/dynamic/db-<id>.yml tồn tại; ConnString chứa "@abc.com:5432/abc?sslmode=require"
}
func TestCreateDatabaseValidation(t *testing.T) // bảng: version "99", name "Abc", domain "localhost", tls_mode "acme", engine "mysql" → errors.Is(err, ErrInvalid); không có lần Run nào
func TestCreateDatabaseRollbackOnUnhealthy(t *testing.T) {
	// healthErr = errors.New("unhealthy") → lỗi; fakeRuntime.removes chứa ("<id>","fridge-postgres-abc"); không còn db-<id>.yml/.crt; row status "error"
	// healthErr = nil, tạo lại "abc" cùng domain → OK
}
func TestCreateDatabaseDefaultSchedule(t *testing.T) // không có destination → 0 schedule; có 2 destination → 1 schedule, DestinationID = id nhỏ nhất, Cron khớp `^\d{1,2} 2 \* \* \*$`, KeepLatest 7, Prefix "fridge"
func TestSetAllowlist(t *testing.T) // "10.0.0.0/8","bad" → ErrInvalid; hợp lệ → _base.yml chứa CIDR mới; Allowlist() trả lại đúng; mặc định (chưa set) trả 3 dải
```

- [ ] **Step 3: `go test ./internal/app/`** → FAIL.
- [ ] **Step 4: Implement.** (`Scheduler` chưa có → khai báo `type Scheduler struct{}` + `Reload` rỗng trong `scheduler.go`, Task 9 điền.)
- [ ] **Step 5: Chạy lại** → PASS.
- [ ] **Step 6: Commit** `feat(app): create postgres database with routing and rollback`.

---

### Task 9: objstore + destinations + backup + scheduler + alert

**Files:**
- Create: `internal/objstore/objstore.go`, `internal/objstore/objstore_test.go`, `internal/objstore/objstore_integration_test.go` (`//go:build integration`), `internal/alert/alert.go`, `internal/alert/alert_test.go`, `internal/app/destinations.go`, `internal/app/backup.go`, `internal/app/backup_test.go`
- Modify: `internal/app/scheduler.go`, `internal/app/app.go` (thêm interface + field), `internal/app/fakes_test.go` (thêm `fakeObjects`; `newTestApp` trả thêm `*fakeObjects`, cập nhật call site Task 8)

**Interfaces:**
- Produces:

```go
// app.go — thêm
type ObjectStore interface { // *objstore.Store thỏa interface này
	Test(ctx) error
	Put(ctx, key string, r io.Reader, meta map[string]string) (int64, error)
	Get(ctx, key string) (io.ReadCloser, error)
	Meta(ctx, key string) (map[string]string, error)
	List(ctx, prefix, q string, limit int) ([]objstore.Object, bool, error)
	Delete(ctx, key string) error
	PresignGet(ctx, key string, ttl time.Duration) (string, error)
}
// field mới trong App:
OpenObjects func(d store.Destination, secretKey string) (ObjectStore, error)
// fakes_test.go: fakeObjects — map key → {data, meta}; field putErr

// objstore
type Object struct { Key string; Size int64; LastModified time.Time }
func New(d store.Destination, secretKey string) (*Store, error) // Endpoint là URL đầy đủ; https → Secure; PathStyle → BucketLookupPath
func FilterSort(objs []Object, q string, limit int) ([]Object, bool) // lọc chứa q (không phân biệt hoa thường), mới nhất trước, cắt limit, bool = bị cắt
// (*Store) thỏa app.ObjectStore. List: liệt kê đệ quy tối đa 1000 object dưới prefix rồi FilterSort; Meta: UserMetadata, key lowercase, bỏ "x-amz-meta-"; Test: put "fridge-test-<rand>" → stat → remove.

// alert
func Webhook(url string) func(ctx context.Context, text string) // url rỗng → chỉ log; POST JSON {"text": text}, timeout 10s, lỗi chỉ log

// app
func (a *App) CreateDestination(ctx, d store.Destination, secretKey string) (store.Destination, error) // name không rỗng, Endpoint parse được http(s), Bucket không rỗng → nếu không ErrInvalid
func (a *App) UpdateDestination(ctx, d store.Destination, secretKey string) error                      // secretKey rỗng = giữ cũ
func (a *App) DeleteDestination(ctx, id int64) error
func (a *App) TestDestination(ctx, id int64) error
func (a *App) ListObjects(ctx, destID int64, prefix, q string) ([]objstore.Object, bool, error)        // limit 200
func (a *App) CreateSchedule(ctx, sc store.Schedule) (store.Schedule, error) // cron.ParseStandard, KeepLatest ≥ 1, destination tồn tại → nếu không ErrInvalid; Reload
func (a *App) UpdateSchedule(ctx, sc store.Schedule) error                   // cùng validate; Reload
func (a *App) DeleteSchedule(ctx, id int64) error                            // Reload
func (a *App) StartBackup(ctx, dbID, destID int64, scheduleID *int64, trigger string) (store.Backup, error)
func (a *App) BackupDownloadURL(ctx, backupID int64) (string, error)         // presign 15 phút; backup không ok → ErrInvalid
func BackupKey(prefix, engine, version, db string, t time.Time, ext string) string
// scheduler
func NewScheduler(a *App) *Scheduler
func (s *Scheduler) Reload(ctx context.Context) error // xóa hết entry, thêm 1 entry / schedule enabled → StartBackup(db, sc.DestinationID, &sc.ID, "schedule"); ErrConflict → log "skipped, backup running"
func (s *Scheduler) Start(); func (s *Scheduler) Stop()
```

`StartBackup`:
- Chọn đích: `scheduleID` != nil → schedule đó (dest + prefix). `destID` > 0 → dest đó, prefix `fridge`. Cả hai trống → schedule đầu tiên của DB (dest + prefix, nhưng `ScheduleID` = nil vì là manual), nếu không có → destination id nhỏ nhất, prefix `fridge`, nếu không có → ErrInvalid "no destination configured".
- DB phải `running`, nếu không → ErrInvalid.
- `Store.StartBackup` với S3Key = `BackupKey(...)` (ErrConflict trả nguyên → API 409). Chạy `runBackup` trong goroutine (`a.wg`), dùng `context.Background()`, trả row ngay.

`runBackup`: `io.Pipe`; goroutine `RT.Exec(DumpCmd, nil, pw, &stderr)` → exit ≠ 0 hoặc err → `pw.CloseWithError(fmt.Errorf("dump exited %d", code))`, ngược lại `pw.Close()`; luồng chính `objects.Put(key, pr, meta)`. Lỗi ở bất kỳ bên nào → `FinishBackup(failed, log=stderr, err)`, gọi `Alert("backup failed: <db>: <err>")`, cố `Delete(key)` (bỏ qua lỗi). Thành công → `FinishBackup(ok, size)`, rồi nếu có schedule: retention — với `OKBackupsForSchedule`, các bản ở vị trí ≥ KeepLatest → `Delete(key)` + `DeleteBackup`. Backup manual không bao giờ bị retention xóa. *(Quyết định của plan)*

- [ ] **Step 1: Viết test**

```go
// objstore_test.go
func TestFilterSort(t *testing.T) // 3 object, q "ABC" khớp 2 → 2, mới nhất trước; limit 1 → 1 object, truncated=true
// alert_test.go
func TestWebhookPostsText(t *testing.T) // httptest server nhận body {"text":"hi"}, Content-Type application/json
// backup_test.go
func TestBackupKey(t *testing.T) // ("fridge/","postgres","17","abc", 2026-10-20 02:00:00 UTC, ".dump") == "fridge/postgres-17/abc/abc-20261020-020000.dump"; prefix "" → không có "/" đầu
func TestBackupHappyPath(t *testing.T) {
	// execFn ghi "DUMPDATA" ra stdout, trả 0; StartBackup(db,0,nil,"manual"); a.Wait()
	// row ok, SizeBytes==8; fakeObjects có key, data "DUMPDATA", meta {"fridge-engine":"postgres","fridge-version":"17","fridge-db":"abc"}
	// execFn nhận cmd[0]=="pg_dump"
}
func TestBackupDumpFailsMidStream(t *testing.T) {
	// execFn ghi "PART" rồi stderr "boom", trả 1 → row failed, Log chứa "boom"; fakeObjects không còn key; Alert nhận 1 text chứa "abc"
}
func TestBackupConcurrent(t *testing.T) // execFn chặn trên channel; StartBackup lần 2 → errors.Is(err, store.ErrConflict); mở channel; a.Wait()
func TestBackupNoDestination(t *testing.T) // ErrInvalid
func TestRetention(t *testing.T) // schedule KeepLatest 2, chạy 3 backup schedule + 1 manual → còn 2 bản schedule mới nhất + bản manual, cả trong store lẫn fakeObjects
func TestCreateScheduleValidation(t *testing.T) // cron "bad" → ErrInvalid; KeepLatest 0 → ErrInvalid
func TestDeleteDestinationInUse(t *testing.T)   // errors.Is(err, store.ErrInUse)
```

```go
// objstore_integration_test.go — cần MinIO (env FRIDGE_IT_S3_ENDPOINT, _ACCESS, _SECRET, _BUCKET; thiếu → t.Skip)
func TestMinioRoundTrip(t *testing.T) // Test() nil; Put với meta → Meta()["fridge-engine"]=="postgres"; Get đọc lại đúng; List(prefix) thấy key; PresignGet tải được bằng http.Get; Delete; sai secret → Test() lỗi
```

- [ ] **Step 2: `go test ./internal/...`** → FAIL.
- [ ] **Step 3: Implement** objstore, alert, destinations, backup, scheduler (robfig/cron `cron.New(cron.WithLocation(time.Local))`).
- [ ] **Step 4: Chạy `go test ./internal/...`** → PASS; `go test -race ./internal/app/` → PASS.
- [ ] **Step 5: Commit** `feat: s3 destinations, scheduled and manual backups with retention and alerts`.

---

### Task 10: app — Restore từ file S3 vào node mới

**Files:**
- Create: `internal/app/restore.go`, `internal/app/restore_test.go`

**Interfaces:**
- Consumes: `CreateDatabase`, `removeNode` (Task 8), `ObjectStore` + `fakeObjects` (Task 9).
- Produces:

```go
func DetectSource(meta map[string]string, key string) (engine, version string)
func CheckCompat(srcEngine, srcVer, dstEngine, dstVer string) (warning string, err error)
type RestoreRequest struct { DestinationID int64; Key, Engine string; New *CreateRequest; TargetDatabaseID int64 }
func (a *App) StartRestore(ctx, req RestoreRequest) (store.Restore, error)
```

- `DetectSource`: thứ tự `meta["fridge-engine"]`/`meta["fridge-version"]` → đoạn đường dẫn khớp `^(postgres|mysql|mariadb|mongo|redis)-(\d+(?:\.\d+)*)$` → đuôi file (`.dump`→postgres, `.rdb`→redis, `.archive.gz`→mongo; `.sql`/`.sql.gz` → ""). Version "" nếu không biết.
- `CheckCompat`: engine khác nhau → cặp mysql/mariadb trả warning `"cross MySQL/MariaDB restore: verify compatibility"`, còn lại ErrInvalid; cùng engine, `srcVer != ""` và `CompareVersions(dstVer, srcVer) < 0` → ErrInvalid `"target version <dst> is older than source <src>"`.
- `StartRestore` (phần đồng bộ, lỗi trả ngay):
  - `TargetDatabaseID != 0` → ErrInvalid "overwrite lands in phase 3"; `New == nil` → ErrInvalid.
  - Destination tồn tại; `Meta(key)` (object không tồn tại → ErrNotFound).
  - Engine: detected `e`, yêu cầu `r`: cả hai rỗng → ErrInvalid "cannot detect engine; pass engine"; cả hai có và khác nhau → ErrInvalid; dùng cái có.
  - Version đích: `New.Version` nếu có, ngược lại version nguồn nếu nằm trong catalog, ngược lại mặc định. `CheckCompat`. Warning ghi vào đầu Log.
  - Kiểm tra trước name/domain chưa bị dùng (`GetDatabaseByName` → ErrConflict).
  - `Store.StartRestore` (mode `new`), chạy phần nền, trả row.
- Phần nền (`a.wg`): `CreateDatabase` → `SetRestoreTarget` → `Get(key)` → (gunzip nếu `RestoreCmd` bảo) → `RT.Exec(RestoreCmd, stdin=stream)`. Lỗi bất kỳ → `FinishRestore(failed, log=stderr, err)` và nếu node đã tạo thì `removeNode`. Thành công → `FinishRestore(ok)`.

- [ ] **Step 1: Viết test**

```go
func TestDetectSource(t *testing.T) {
	// meta {"fridge-engine":"postgres","fridge-version":"16"} → postgres,16
	// key "fridge/postgres-17/abc/abc-1.dump", meta rỗng → postgres,17
	// "manual/x.dump" → postgres,""; "manual/x.sql.gz" → "",""
}
func TestCheckCompat(t *testing.T) // (pg 17 → pg 14) ErrInvalid chứa "older"; (pg 14 → pg 17) ok; (pg "" → pg 13) ok; (mysql 8.4 → mariadb 11.4) warning != ""; (postgres → redis) ErrInvalid
func TestRestoreHappyPath(t *testing.T) {
	// fakeObjects: "fridge/postgres-14/abc/abc-1.dump" data "DUMP", meta engine postgres v14
	// StartRestore{New:{Name:"abc2",Domain:"abc2.com",Version:"17"}}; a.Wait()
	// restore ok; TargetDatabaseID trỏ db "abc2" running version 17; execFn nhận cmd[0]=="pg_restore" và stdin == "DUMP"
}
func TestRestoreBlocksDowngrade(t *testing.T) // nguồn v17, New.Version "14" → ErrInvalid ngay, không có Run nào
func TestRestorePlainSQLGzNeedsEngine(t *testing.T) {
	// key "upload/x.sql.gz" (data = gzip("select 1;")), Engine "" → ErrInvalid
	// Engine "postgres" → ok; execFn nhận cmd[0]=="psql", stdin == "select 1;" (đã giải nén)
}
func TestRestoreGarbageRollsBack(t *testing.T) {
	// execFn trả exit 1 + stderr "invalid input" → restore failed, Log chứa "invalid input"; node mới status "error"; fakeRuntime.removes có container của nó
}
func TestRestoreOverwriteRejected(t *testing.T) // TargetDatabaseID 1 → ErrInvalid
```

- [ ] **Step 2: `go test ./internal/app/`** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: `go test -race ./internal/app/`** → PASS.
- [ ] **Step 5: Commit** `feat(app): restore s3 backups into a new node`.

---

### Task 11: REST API

**Files:**
- Create: `internal/api/api.go`, `internal/api/api_test.go`
- Modify: `internal/app/app.go` (thêm `Ready`)

**Interfaces:**
- Consumes: mọi method public của `app.App` từ Task 8–10; `Store.Ping` (Task 2), `Runtime.Ping` (Task 8).
- Produces: `api.New(a *app.App, token string) http.Handler`; `func (a *App) Ready(ctx) error` (lỗi nếu `Store.Ping` hoặc `RT.Ping` lỗi, timeout 3s).

**Không cần token**: `GET /healthz` → 200 `ok`; `GET /readyz` → 200 `ok` hoặc 503 `not ready` (không lộ chi tiết lỗi).

Route (Go 1.22 `http.ServeMux` pattern). `{db}` là id (số) hoặc name.
```
GET  /api/engines                      → engine.List()
POST /api/databases                    {engine,name,domain,version,tls_mode} → 201 db+connection_string
GET  /api/databases                    → []db (không có connection_string)
GET  /api/databases/{db}               → db+connection_string
GET|POST   /api/destinations           POST {name,provider,endpoint,region,bucket,access_key,secret_key,path_style}
PUT|DELETE /api/destinations/{id}
POST /api/destinations/{id}/test       → 200 {"ok":true} | 400 {"error": "..."}
GET  /api/destinations/{id}/objects?prefix=&q= → {"objects":[...],"truncated":bool}
GET|POST   /api/databases/{db}/schedules
PUT|DELETE /api/schedules/{id}
GET  /api/databases/{db}/backups
POST /api/databases/{db}/backups       {destination_id?} → 202 backup
GET  /api/backups/{id}
GET  /api/backups/{id}/download        → {"url": "..."}
POST /api/restores                     {destination_id,key,engine?,new:{name,domain,version?}} → 202 restore
GET  /api/restores/{id}
GET|PUT /api/settings/allowlist        {"cidrs":[...]}
GET  /api/ca                           → CA PEM (text/plain)
```
- Mọi route cần `Authorization: Bearer <token>`, so sánh `subtle.ConstantTimeCompare`; sai/thiếu → 401.
- Lỗi → JSON `{"error": msg}`: `app.ErrInvalid` 400, `store.ErrNotFound` 404, `store.ErrConflict`/`ErrInUse` 409, còn lại 500. Body JSON sai → 400.
- JSON destination **không bao giờ** có `secret_key`/`secret_key_enc`; JSON database không bao giờ có `password_enc`. Field JSON snake_case.
- *(Quyết định của plan)* Bỏ phân trang `cursor` của listing object: trả ≤ 200 object + `truncated`.

- [ ] **Step 1: Viết test** (dựng `app.App` thật với fake runtime/objects — export helper `apptest` không cần; viết fake tối thiểu ngay trong `api_test.go`)

```go
func TestAuthRequired(t *testing.T)        // không header → 401; sai token → 401; đúng → 200 trên /api/engines
func TestHealthEndpoints(t *testing.T)     // /healthz không token → 200; /readyz → 200 khi fake runtime Ping nil; fake Ping lỗi → 503, body không chứa text lỗi
func TestErrorMapping(t *testing.T)        // POST /api/databases version "99" → 400; GET /api/databases/nope → 404; tạo trùng name → 409; body "{" → 400
func TestNoSecretsInResponses(t *testing.T) // tạo destination secret "TOPSECRET" → body POST/GET list không chứa "TOPSECRET" và không có key "secret_key"; GET /api/databases không chứa "password_enc"
func TestDatabaseByNameOrID(t *testing.T)  // GET /api/databases/abc và /api/databases/<id> trả cùng id
func TestBackupNowAccepted(t *testing.T)   // có destination, POST /api/databases/abc/backups → 202, body có "id" và "status":"running"
```

- [ ] **Step 2: `go test ./internal/api/`** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Chạy lại** → PASS.
- [ ] **Step 5: Commit** `feat(api): rest api with bearer auth`.

---

### Task 12: CLI

**Files:**
- Create: `cmd/fridge/cli.go`, `cmd/fridge/cli_test.go`
- Create (khung): `cmd/fridge/main.go` — `switch os.Args[1]`: `server` (Task 13), còn lại → CLI.

**Interfaces:**
- Consumes: API Task 11 qua HTTP. Env `FRIDGE_URL` (mặc định `http://localhost:8080`), `FRIDGE_TOKEN`.
- Produces: `func runCLI(args []string, stdout, stderr io.Writer) int` (exit code); `func parseFrom(s string) (dest, key string, err error)` (tách ở dấu `:` đầu tiên; thiếu phần nào → lỗi).

Lệnh (stdlib `flag`, mỗi lệnh 1 FlagSet; output dạng bảng `text/tabwriter`, `--json` in JSON thô):
```
fridge engines
fridge create <engine> <name> --domain D [--version V]       → in connection string
fridge ls | fridge show <db>
fridge dest add <name> --endpoint URL --bucket B --access-key A --secret-key S [--region R] [--provider P] [--path-style]
fridge dest ls | fridge dest test <name|id> | fridge dest rm <name|id>
fridge objects <dest> [--prefix P] [--q Q]
fridge schedule add <db> --dest <name|id> [--cron "0 2 * * *"] [--prefix fridge] [--keep 7]
fridge schedule ls <db> | fridge schedule rm <id>
fridge backup <db> [--dest <name|id>] [--wait]                → --wait: poll GET /api/backups/{id} mỗi 2s tới khi khác running; exit 1 nếu failed
fridge backups <db>
fridge restore --from <dest>:<key> --name N --domain D [--version V] [--engine E] [--wait]
fridge allowlist | fridge allowlist set <cidr>...
fridge ca                                                      → in PEM
```
Tên destination → id bằng cách gọi `GET /api/destinations` rồi khớp `name` (chuỗi số thì coi là id).

- [ ] **Step 1: Viết test**

```go
func TestParseFrom(t *testing.T) // "minio2:fridge/postgres-17/abc/a.dump" → ("minio2","fridge/postgres-17/abc/a.dump"); "nokey" lỗi; ":k" lỗi; "d:" lỗi
func TestCreateCallsAPI(t *testing.T) {
	// httptest server ghi lại request; FRIDGE_URL trỏ tới nó; runCLI(["create","postgres","abc","--domain","abc.com","--version","16"])
	// → POST /api/databases, body {"engine":"postgres","name":"abc","domain":"abc.com","version":"16"}, header Bearer; exit 0; stdout chứa connection_string server trả về
}
func TestAPIErrorExitCode(t *testing.T) // server trả 400 {"error":"bad version"} → exit 1, stderr chứa "bad version"
func TestRestoreResolvesDestinationName(t *testing.T) // GET /api/destinations trả [{id:3,name:"minio2"}] → POST /api/restores có destination_id 3, key đúng
```

- [ ] **Step 2: `go test ./cmd/fridge/`** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Chạy lại** → PASS.
- [ ] **Step 5: Commit** `feat(cli): fridge command line client`.

---

### Task 13: Server wiring + deploy + kiểm thử end-to-end

**Files:**
- Modify: `cmd/fridge/main.go` (lệnh `server`)
- Create: `deploy/Dockerfile`, `deploy/docker-compose.yml`, `deploy/traefik.yml`, `internal/app/e2e_integration_test.go` (`//go:build integration`), `README.md` (chỉ phần chạy thử + biến môi trường)

**Interfaces:**
- Consumes: tất cả.

`fridge server` đọc env: `FRIDGE_DATABASE_URL` (bắt buộc), `FRIDGE_LISTEN` (`:8080`), `FRIDGE_DATA` (`/data`, chứa CA), `FRIDGE_TRAEFIK_DIR` (`/traefik`), `FRIDGE_NETWORK` (`fridge-net`), `FRIDGE_API_BACKEND` (`http://fridge:8080`), `FRIDGE_SECRET_KEY` (bắt buộc), `FRIDGE_TOKEN` (bắt buộc), `FRIDGE_ALERT_WEBHOOK` (tùy chọn). Thiếu biến bắt buộc → thoát kèm thông báo rõ.
Thứ tự khởi động: `store.Open(ctx, FRIDGE_DATABASE_URL)` (retry tối đa 30s chờ `fridge-meta` sẵn sàng) → `FailRunning("interrupted by restart")` → `container.New` + `EnsureNetwork` → `pki.LoadOrCreate(<data>/ca)` → tạo thư mục `<traefik>/dynamic`, `<traefik>/certs` → `proxy.WriteBase(Allowlist())` → `NewScheduler` + `Reload` + `Start` → `http.Server` với `api.New`. SIGINT/SIGTERM → `Shutdown`, `Sched.Stop`, `a.Wait()` tối đa 30s. `HealthTimeout` = 120s.

`deploy/traefik.yml`: entryPoints `pg: ":5432"`, `api: ":8080"`, `ping: ":8082"`; `ping: {entryPoint: ping}` (**bắt buộc** chỉ định — mặc định ping dùng entrypoint nội bộ `traefik` ở `:8080`, trùng `api`); `providers.file {directory: /traefik/dynamic, watch: true}`; log INFO.

`fridge healthcheck` (lệnh con mới trong `main.go`): GET `http://127.0.0.1<FRIDGE_LISTEN>/readyz`, exit 0 nếu 200, ngược lại 1. Dùng cho healthcheck compose vì image distroless không có curl/wget.

`deploy/docker-compose.yml`:
- `traefik`: image `traefik:v3.5` (hoặc tag v3 ổn định mới nhất, pin cụ thể), ports `5432:5432`, `8080:8080`, mount `./traefik.yml:/etc/traefik/traefik.yml:ro` + volume `traefik:/traefik`, network `fridge-net`, `restart: always`, healthcheck `traefik healthcheck --ping`.
- `fridge-meta`: `postgres:17`, env `POSTGRES_DB=fridge`, `POSTGRES_USER=fridge`, `POSTGRES_PASSWORD=${FRIDGE_META_PASSWORD}`, volume `fridge-meta:/var/lib/postgresql/data`, network `fridge-net`, healthcheck `pg_isready -U fridge -d fridge`, `restart: always`, không publish port.
- `fridge`: build `..` với `deploy/Dockerfile`, `depends_on: fridge-meta (condition: service_healthy)`, env như trên (đọc từ `.env`; `FRIDGE_DATABASE_URL=postgres://fridge:${FRIDGE_META_PASSWORD}@fridge-meta:5432/fridge?sslmode=disable`), volume `fridge-data:/data`, `traefik:/traefik`, `/var/run/docker.sock:/var/run/docker.sock`, network `fridge-net`, `restart: unless-stopped`, healthcheck `["CMD","/fridge","healthcheck"]`, không publish port (đi qua Traefik `api`).
- `minio` (profile `dev`): `minio/minio server /data --console-address :9001`, ports `9000`, `9001`, network `fridge-net`.
- `networks.fridge-net.name: fridge-net`.

`deploy/Dockerfile`: build `golang:1.26` với `CGO_ENABLED=0` → chạy trên `gcr.io/distroless/static`, `ENTRYPOINT ["/fridge","server"]`.

- [ ] **Step 1: Viết integration test end-to-end** (cần Docker + MinIO; dùng `container.Client` và `objstore` thật)

```go
func TestE2EBackupRestoreAcrossVersions(t *testing.T) {
	// App thật: RT = container.New("fridge-it-net"), OpenObjects = objstore.New, TraefikDir tạm
	// tạo destination MinIO (env như Task 9), CreateDatabase postgres "it14" version 14
	// RT.Exec psql: create table t(x int); insert 3 dòng
	// StartBackup → Wait → ok; object có trong MinIO
	// StartRestore → new "it17" version 17 → Wait → ok; psql "select count(*) from t" trên it17 == "3"
	// StartRestore key đó → new version "13" → ErrInvalid
	// dọn: removeNode cả hai
}
```

- [ ] **Step 2: Chạy** `go test -tags integration ./internal/app/ -run E2E -v` (trong WSL; chạy MinIO bằng `docker compose -f deploy/docker-compose.yml --profile dev up -d minio`, thêm port `9000` publish ra host để test từ WSL gọi `http://localhost:9000`) → FAIL trước khi wiring, PASS sau Step 3.
- [ ] **Step 3: Implement** `server` + 3 file deploy + README.
- [ ] **Step 4: `go build ./... && go vet ./... && go test ./...`** → tất cả PASS.
- [ ] **Step 5: Smoke test thủ công qua compose, chạy trong WSL** (CLI build bằng `go build -o /tmp/fridge ./cmd/fridge`; `FRIDGE_URL=http://localhost:8080`; ghi kết quả vào mô tả commit):
  1. Tạo `deploy/.env` có `FRIDGE_SECRET_KEY` (`openssl rand -base64 32`), `FRIDGE_TOKEN`, `FRIDGE_META_PASSWORD`; `docker compose -f deploy/docker-compose.yml --profile dev up -d --build`. Tạo bucket `fridge` trên MinIO console.
  2. `fridge dest add local --endpoint http://minio:9000 --bucket fridge --access-key minioadmin --secret-key minioadmin --path-style` → `fridge dest test local` OK.
  3. `fridge create postgres abc --domain abc.com --version 16`, `fridge create postgres xyz --domain xyz.com`.
  4. Client chạy trong Docker để có SNI: `docker run --rm --add-host abc.com:host-gateway postgres:17 psql "<conn abc>" -c "create table only_abc(x int)"`; làm tương tự với xyz và `\dt` → mỗi bên chỉ thấy bảng của mình qua **cùng port 5432**.
  5. `fridge backup abc --wait` → `fridge objects local --prefix fridge/postgres-16` thấy file → `fridge restore --from local:<key> --name abc2 --domain abc2.com --version 17 --wait` → `abc2` có bảng `only_abc`.
  6. `fridge allowlist set 203.0.113.0/24` → psql tới abc.com bị từ chối; `fridge allowlist set 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16` → vào lại được.
  7. `docker compose stop fridge` → psql tới abc.com vẫn chạy (data plane độc lập).
  8. Đổi secret destination sang sai rồi `fridge backup abc --wait` → exit 1; nếu đặt `FRIDGE_ALERT_WEBHOOK` (vd webhook.site) thì nhận được alert.
- [ ] **Step 6: Commit** `feat: fridge server wiring and docker-compose deployment`.
