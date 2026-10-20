# Fridge — Thiết kế hệ thống tạo, quản lý, backup nhiều loại DB với routing theo domain

## Context

Team nội bộ (dev/ops) hiện tạo DB bằng tay/docker-compose cho từng dự án. Cần một công cụ tự phục vụ: tạo DB cho một domain (vd `abc.com` → DB1, `xyz.com` → DB2) qua Web UI / CLI / API, tự backup hằng ngày, restore dễ, và mọi DB đi chung vài port nhờ routing theo SNI. Thư mục `d:\Windows\Desktop\code\Fridge` đang trống — dự án mới hoàn toàn. Đã cân nhắc Coolify/Dokploy, người dùng chọn **tự xây**.

## Mục tiêu

- Tạo / xem / xóa DB thuộc 5 loại: **PostgreSQL, MySQL, MariaDB, MongoDB, Redis**.
- Routing theo domain:
  - **Postgres, Mongo, Redis**: mỗi loại 1 port chung, route theo **SNI** (TLS).
  - **MySQL, MariaDB**: chung port 3306, route theo **username** qua ProxySQL (giao thức MySQL/MariaDB bắt tay plaintext trước TLS nên không đọc được SNI — đã chốt với người dùng).
- Backup **dump hằng ngày** lên kho S3-compatible, có retention.
- **Backup & restore 1 click kiểu Dokploy**: quản lý nhiều **S3 Destination**, mỗi DB gắn lịch backup vào destination; nút "Backup now"; restore bằng cách duyệt file trong bucket → chọn → bấm Restore, log chạy trực tiếp.
- **Chọn version** cho từng loại DB khi tạo node DB (vd Postgres 14/15/16/17/18).
- 3 giao diện: **Web UI, CLI, REST API** (CI gọi được).
- Giai đoạn 1 chạy **1 server + Docker**; thiết kế chừa đường cho nhiều server sau.

## Non-goals (giai đoạn này)

- PITR / WAL / binlog streaming (chấp nhận mất tối đa ~1 ngày).
- HA / replication của từng DB.
- Multi-server thực sự (chỉ chừa cột `host_id`, không viết agent).
- Billing, quota, multi-tenant cho khách ngoài.

## Anti-goals (thất bại dù vẫn chạy)

1. **Backup âm thầm hỏng** → bắt buộc: alert khi backup fail, alert khi DB không có backup thành công > 26h, và **tự test-restore** định kỳ.
2. **Ai cũng xóa được DB** → phân quyền admin/member, xóa là soft-delete, audit log.
3. **Proxy chết kéo sập hết** → data plane (Traefik, ProxySQL, container DB) độc lập với Fridge; Fridge chết thì DB vẫn truy cập được. Proxy có healthcheck + `restart: always` + alert.
4. **Phức tạp hơn làm tay** → tạo DB = 1 lệnh, trả về connection string dùng được ngay.

## Kiến trúc

```
                 ┌──────────── Fridge (Go binary) ────────────┐
 Web UI / CLI ──▶│ REST API · Web UI (embed) · Scheduler       │
                 │ SQLite (metadata) · Docker SDK · S3 client  │
                 └───────┬───────────────┬──────────────┬──────┘
          writes yaml    │   admin SQL   │  docker API  │ upload dumps
                         ▼               ▼              ▼
 :5432 :27017 :6379 ┌─────────┐ 3306   ┌──────────┐  ┌─────────┐   MinIO
 (SNI) + :3306 ───▶ │ Traefik │ ─────▶ │ ProxySQL │  │ DB ctrs │   (server khác)
 (IP allowlist)     └────┬────┘        └────┬─────┘  └─────────┘
                         └──── docker network: fridge-net ────┘
```

- **Control plane** = Fridge. **Data plane** = Traefik + ProxySQL + container DB. Data plane không phụ thuộc Fridge đang chạy.
- Container DB **không publish port ra host**; chỉ reachable qua `fridge-net` sau proxy.

### Stack (lựa chọn của Claude, người dùng giao quyền)

- **Go**, 1 binary `fridge`: `fridge server` (API + UI + scheduler) và các lệnh CLI gọi API.
- **SQLite** (`modernc.org/sqlite`, pure Go) cho metadata.
- **Docker Engine SDK** (`github.com/docker/docker/client`).
- **Traefik v3** (TCP routers, `HostSNI`, có hỗ trợ Postgres STARTTLS) — file provider, Fridge ghi file dynamic, Traefik watch.
- **ProxySQL 2.x** — Fridge cấu hình qua admin interface (port 6032, giao thức MySQL).
- **minio-go** cho S3-compatible (MinIO local / R2 / S3 / Wasabi...).
- Web UI: Go `html/template` + **htmx**, nhúng bằng `embed`. Không SPA.
- CLI: `cobra` hoặc stdlib `flag` (builder chọn).

## Thành phần chi tiết

### 1. Mô hình dữ liệu (SQLite)

- `users(id, email, password_hash, role[admin|member], created_at)`
- `api_tokens(id, user_id, token_hash, name, last_used_at)`
- `hosts(id, name, docker_endpoint)` — giai đoạn 1 chỉ 1 hàng `local`.
- `databases(id, name, engine, version, domain, tls_mode[acme|internal], db_user, password_enc, host_id, container_id, volume, status[creating|running|stopped|deleted|error], created_by, created_at, deleted_at)`
  - `version` = tag major/minor lấy từ catalog (mục 2), cố định sau khi tạo.
- `destinations(id, name, provider[s3|minio|r2|wasabi|other], endpoint, region, bucket, access_key, secret_key_enc, path_style, created_at)` — nhiều destination, secret mã hóa AES-GCM.
- `backup_schedules(id, database_id, destination_id, cron, prefix, keep_latest, enabled)` — 1 DB có thể có nhiều lịch (vd hằng ngày → MinIO, hằng tuần → R2).
  - `domain` unique theo nhóm SNI (pg/mongo/redis); `db_user` unique chung cho MySQL+MariaDB (ProxySQL map theo username).
- `backups(id, database_id, destination_id, schedule_id?, trigger[schedule|manual|pre-delete|pre-restore], engine, engine_version, status[running|ok|failed], started_at, finished_at, size_bytes, s3_key, log, error, verified_at, verify_status)`
- `restores(id, destination_id, s3_key, source_engine_version?, target_database_id, mode[new|overwrite], status, log, started_by, started_at, finished_at)`
- `audit_log(id, user_id, action, target, detail, at)`
- `settings(key, value)` — allowlist CIDR, webhook, ACME provider.
- Mật khẩu DB mã hóa AES-GCM, khóa từ env `FRIDGE_SECRET_KEY`.

### 2. Engine (1 file / engine, 1 interface nhỏ — có 5 implementation nên đáng có)

Mỗi engine cung cấp: image + env + volume path, cách tạo user/db ban đầu, lệnh dump (stream stdout), lệnh restore (đọc stdin), lệnh sanity check sau restore, kiểu route (`sni` hoặc `proxysql`), port nội bộ.

| Engine | Image mặc định | Dump (qua `docker exec`, stdout) | Route |
|---|---|---|---|
| Postgres | `postgres:17` | `pg_dump -Fc` | Traefik SNI :5432 (STARTTLS) |
| MySQL | `mysql:8.4` | `mysqldump --single-transaction --routines` | ProxySQL :3306 theo user |
| MariaDB | `mariadb:11` | `mariadb-dump --single-transaction --routines` | ProxySQL :3306 theo user |
| Mongo | `mongo:7` | `mongodump --archive --gzip` | Traefik SNI :27017 |
| Redis | `redis:7` | `redis-cli --rdb -` | Traefik SNI :6379 |

- Mỗi DB = 1 container riêng `fridge-<engine>-<name>` + named volume riêng (cô lập, backup/restore độc lập). *(Phỏng đoán của Claude, chưa được người dùng xác nhận.)*
**Chọn version (catalog)**
- Mỗi engine khai báo danh sách version hỗ trợ + version mặc định ngay trong code (`internal/engine/catalog.go`), vd:
  - Postgres: `13, 14, 15, 16, 17, 18` (mặc định 17)
  - MySQL: `8.0, 8.4` (mặc định 8.4)
  - MariaDB: `10.6, 10.11, 11.4, 11.8` (mặc định 11.4)
  - Mongo: `6.0, 7.0, 8.0` (mặc định 7.0)
  - Redis: `6.2, 7.2, 7.4, 8.0` (mặc định 7.4)
  - Builder cập nhật danh sách theo tag thực tế trên Docker Hub lúc làm.
- Image = `<engine-image>:<version>` (tag major/minor, nhận patch mới khi pull lại). Engine nào khác biệt theo version (vd MySQL 8.0 dùng `mysqldump`, MariaDB ≥ 11 dùng `mariadb-dump`, Redis 6.2 không có ACL mới) thì xử lý trong file engine đó.
- UI: dropdown version theo engine đã chọn; CLI `--version 16`; API `version` (thiếu → mặc định). Version ngoài catalog → 400.
- `GET /api/engines` trả catalog để UI/CLI hiển thị.
- **Không** nâng version tại chỗ. Muốn lên version: tạo node mới version cao hơn → restore backup từ S3 vào (dùng chính luồng 1 click ở mục 5). *(Lựa chọn của Claude)*
- *(Lựa chọn của Claude)* Chưa cho admin nhập image tag tùy ý; thêm khi catalog không đủ.

### 3. Routing

**Traefik (Postgres/Mongo/Redis)**
- Entrypoints `pg:5432`, `mongo:27017`, `redis:6379`.
- Mỗi DB → 1 file `traefik/dynamic/<db-id>.yml`: TCP router `HostSNI(\`abc.com\`)` → service `fridge-postgres-abc:5432`. Traefik **terminate TLS**, backend nói plaintext trong `fridge-net`.
- Ghi file theo kiểu atomic (ghi tạm → rename) để Traefik không đọc file dở.
- Client: `psql "host=abc.com sslmode=require"`, `mongodb://...@abc.com:27017/?tls=true`, `rediss://:pass@abc.com:6379`.

**ProxySQL (MySQL/MariaDB)**
- Mỗi DB → 1 hostgroup; `mysql_servers` (hostgroup, container host:3306), `mysql_users` (username, password, default_hostgroup); `LOAD ... TO RUNTIME; SAVE ... TO DISK`.
- Monitor user của ProxySQL được tạo trong mỗi DB MySQL/MariaDB lúc khởi tạo.
- Client: `mysql -h abc.com -P 3306 -u abc_app -p` (domain chỉ để gọi cho dễ nhớ; route thật theo user).

**TLS cert** — hỗ trợ **cả hai**, chọn theo từng DB (cột `tls_mode`):
- `acme`: domain public → Traefik `certResolver` Let's Encrypt **DNS-01** (provider + token cấu hình 1 lần, vd Cloudflare).
- `internal`: domain nội bộ → Fridge tự sinh **internal CA** lần đầu (`crypto/x509` stdlib, lưu cạnh SQLite), ký cert lá cho từng domain, ghi vào phần `tls.certificates` của file dynamic Traefik. `fridge ca` / nút UI tải CA cert để team cài 1 lần. Tự gia hạn cert lá trước hạn 30 ngày (job trong scheduler).

**IP allowlist** (người dùng chốt):
- Danh sách CIDR toàn cục, mặc định 3 dải private: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`. Admin sửa được qua UI/API/CLI.
- Áp bằng middleware **Traefik `ipAllowList`** cho mọi router (TCP + HTTP Web UI/API).
- Để 3306 cũng đi qua allowlist: Traefik entrypoint `mysql:3306` với router `HostSNI(\`*\`)` (TCP thô) → ProxySQL. ProxySQL **không** publish port ra host.
- *(Lựa chọn của Claude)* Chỉ làm allowlist toàn cục; override theo từng DB để sau khi có nhu cầu.

### 4. Vòng đời DB

- **Create**: validate → ghi DB `creating` → tạo volume + container (healthcheck) → chờ healthy → tạo user/db → ghi route (Traefik file / ProxySQL) → `running` → trả connection string.
  Lỗi giữa chừng → rollback (xóa container/volume/route), status `error`.
- **Delete (admin)**: chạy backup cuối → stop + remove container → gỡ route → giữ volume + backup thêm 7 ngày (soft-delete) → job dọn sau đó. Bắt buộc gõ lại tên DB để xác nhận (UI & CLI).
- **Reconcile** khi Fridge khởi động và mỗi 5 phút: so trạng thái mong muốn (SQLite) với Docker + file Traefik + ProxySQL runtime, sửa lệch, log/alert.

### 5. Backup & restore

Mô phỏng luồng của Dokploy: **Destinations → Schedules → Backup now → Restore từ file trong bucket**.

**S3 Destinations** (admin quản lý, trang Settings → Destinations)
- Thêm/sửa/xóa destination: name, provider, endpoint, region, bucket, access/secret key, path-style.
- Nút **Test connection**: thử put + list + delete 1 object nhỏ, báo lỗi rõ (sai key, sai bucket, không tới endpoint).
- Đích chính của người dùng: **MinIO trên server khác**. MinIO trong `deploy/docker-compose.yml` chỉ dùng cho dev/test.
- Không cho xóa destination khi còn schedule trỏ tới nó.

**Backup**
- Tab **Backups** của mỗi DB: danh sách schedule (destination, cron, prefix, keep latest, bật/tắt) + lịch sử backup (thời gian, size, trạng thái, verified, log).
- Lúc tạo DB: tự tạo 1 schedule mặc định (02:00 hằng ngày, keep 7) vào destination mặc định nếu có. Rải lệch giờ để không chạy dồn.
- Nút **Backup now** (1 click): chọn schedule/destination (mặc định cái đầu tiên) → chạy ngay, log stream về UI.
- Luồng: `docker exec` lệnh dump → stream stdout → gzip (nếu engine chưa nén) → S3 multipart upload. Không ghi đệm ra đĩa.
- Key object: `<prefix>/<engine>-<version>/<db-name>/<db-name>-<YYYYMMDD-HHMMSS>.<ext>` (vd `fridge/postgres-17/abc/abc-20261020-020000.dump`). Ghi thêm S3 object metadata `x-amz-meta-fridge-engine`, `-version`, `-db`. Nhờ vậy file vẫn tự mô tả được khi đọc ngoài Fridge.
- Retention theo từng schedule: giữ `keep_latest` bản mới nhất có `ok` dưới prefix của schedule; chỉ xóa sau khi bản mới upload xong.
- Nút **Download** (presigned URL) cho từng bản.
- **Test-restore** (chống anti-goal #1): hằng tuần mỗi DB (cấu hình được), tạo container tạm, restore bản mới nhất, chạy sanity check (vd đếm bảng/collection/key > 0 hoặc khớp số liệu lúc dump), xóa container tạm, ghi `verified_at`.
**Restore 1 click từ S3**
- Nút **Restore** trên trang DB, hoặc trang Restore chung:
  1. Chọn **destination** → Fridge list object trong bucket (lọc theo prefix, ô search theo tên, sắp mới nhất trước, phân trang).
  2. Chọn **file**. Được chọn cả file không do Fridge tạo, ví dụ dump upload tay lên bucket. Engine/version đoán từ metadata, nếu không có thì từ đường dẫn, nếu vẫn không có thì từ đuôi file. Không đoán được thì bắt người dùng chọn engine.
  3. Chọn **đích**: *node mới* (mặc định; chọn version, gợi ý = version nguồn) hoặc *ghi đè node hiện tại*.
  4. Bấm **Restore** → log stream trực tiếp (SSE), trạng thái lưu vào bảng `restores`.
- Luồng: S3 GetObject → stream → `docker exec -i` lệnh restore (`pg_restore`, `mysql`/`mariadb`, `mongorestore --archive --gzip`; Redis: dừng → thay `dump.rdb` → khởi động). Không ghi đệm ra đĩa trừ Redis.
- Kiểm tra tương thích trước khi chạy:
  - Engine của file ≠ engine đích → chặn.
  - Version đích **thấp hơn** version nguồn (pg_restore cũ không đọc được dump mới, RDB mới không load được vào Redis cũ) → chặn, báo lý do.
  - MySQL ↔ MariaDB chéo nhau → cho phép, kèm cảnh báo.
- Quyền:
  - Restore vào node mới: member làm được.
  - Ghi đè node hiện tại: chỉ admin. Fridge tự backup `pre-restore` trước khi ghi đè, và phải gõ tên DB để xác nhận.
- CLI/API dùng chung luồng: `fridge restore --from <destination>:<key> --to new|<db>`.
- **Alert**: webhook chung (JSON, tương thích Slack/Telegram/Discord qua cấu hình template) khi: backup fail, test-restore fail, DB không có backup ok > 26h, proxy unhealthy, reconcile phát hiện lệch.

### 6. Auth & quyền

- Login email/password (UI, session cookie); API token cho CLI/CI (`Authorization: Bearer`).
- **member**: tạo DB, xem, lấy connection string, chạy backup thủ công, restore-vào-DB-mới.
- **admin**: thêm toàn bộ: xóa DB, restore ghi đè, quản lý user/token, cấu hình S3/alert.
- Mọi hành động ghi `audit_log`.
- Lần chạy đầu: `fridge init` tạo admin đầu tiên.

### 7. API & CLI (bề mặt tối thiểu)

```
GET    /api/engines                             catalog engine + version
POST   /api/databases                           {engine, name, domain, version?, tls_mode}
GET    /api/databases[/{id}]
DELETE /api/databases/{id}

GET|POST        /api/destinations
PUT|DELETE      /api/destinations/{id}
POST            /api/destinations/{id}/test
GET             /api/destinations/{id}/objects?prefix=&q=&cursor=

GET|POST        /api/databases/{id}/schedules
PUT|DELETE      /api/schedules/{id}
GET             /api/databases/{id}/backups
POST            /api/databases/{id}/backups     {destination_id?}   (Backup now)
GET             /api/backups/{id}/download      → presigned URL

POST   /api/restores        {destination_id, s3_key, target: {database_id} | {new: {name, domain, version}}, engine?}
GET    /api/restores/{id}
GET    /api/jobs/{kind}/{id}/log                (SSE stream log backup/restore)
```
CLI tương ứng: `fridge create postgres abc --domain abc.com --version 16`, `fridge engines`, `fridge ls`, `fridge rm abc`, `fridge dest add|ls|test`, `fridge backup abc [--dest minio2]`, `fridge backups abc`, `fridge restore --from minio2:fridge/postgres-17/abc/abc-20261020-020000.dump --to new --name abc2 --domain abc2.com`, `fridge login`.

### 8. Đường lên nhiều server (chỉ chừa chỗ, không làm)

- Cột `host_id` + bảng `hosts` có `docker_endpoint` (tcp+TLS hoặc ssh).
- Khi scale: container DB trên host khác publish port nội bộ (private network/WireGuard) và Traefik/ProxySQL trỏ `host:port` thay vì tên container. Không cần đổi model dữ liệu.

## Cấu trúc repo

```
cmd/fridge/main.go
internal/store/      # SQLite, migrations
internal/engine/     # engine.go (interface) + postgres.go mysql.go mariadb.go mongo.go redis.go
internal/docker/     # bọc Docker SDK: create/exec/stream
internal/proxy/      # traefik.go (ghi yaml), proxysql.go (admin SQL)
internal/backup/     # scheduler, dump→S3, retention, verify
internal/api/        # REST handlers, auth middleware
internal/web/        # templates + htmx, embed
internal/alert/      # webhook
deploy/docker-compose.yml   # fridge, traefik, proxysql (+ minio cho dev)
deploy/traefik.yml          # static config
```

## Giai đoạn triển khai

1. **Xương sống với Postgres**: store, Docker, engine Postgres có catalog version, Traefik SNI route + IP allowlist, internal CA, API + CLI, S3 destinations + test, schedule + Backup now, restore từ file S3 vào node mới, alert backup fail. Chạy được end-to-end.
2. **Thêm 4 engine**: Mongo, Redis (Traefik SNI), MySQL, MariaDB (ProxySQL).
3. **An toàn**: ACME DNS-01, test-restore định kỳ, auth + role + audit, soft-delete, restore ghi đè, reconcile loop, alert proxy/no-backup-26h.
4. **Web UI** (htmx) phủ các luồng trên, gồm: dropdown version, trang Destinations, tab Backups, trình duyệt file S3 + Restore 1 click có log trực tiếp.

## Để builder tự quyết

Thư viện CLI, router HTTP (stdlib `net/http` mux Go 1.22 là đủ), định dạng migration, giao diện cụ thể của UI, cấu trúc log, tên biến cấu hình.

## Verification (end-to-end)

1. `docker compose -f deploy/docker-compose.yml up -d` (kèm MinIO); thêm `abc.com`, `xyz.com` vào hosts file → `127.0.0.1`; dùng cert tự ký/internal CA cho test.
2. `fridge create postgres abc --domain abc.com` và `fridge create postgres xyz --domain xyz.com` → `psql "host=abc.com sslmode=require ..."` và `host=xyz.com` vào **đúng 2 DB khác nhau** cùng port 5432 (tạo bảng khác nhau để kiểm chứng).
3. Lặp với Mongo (`mongosh --tls`), Redis (`redis-cli --tls --sni`), MySQL/MariaDB (`mysql -u abc_app` vs `-u xyz_app` cùng port 3306).
4. `fridge backup abc` → thấy object trong MinIO; `fridge restore abc <id>` → DB mới có cùng dữ liệu (so số dòng).
5. Làm hỏng backup (vd sai creds S3) → nhận webhook alert.
6. Dừng `fridge server` → các DB vẫn kết nối được qua proxy.
7. Đăng nhập member → `DELETE` trả 403.
8. **Version**: tạo `postgres 14` và `postgres 17`, `SELECT version()` đúng từng node; `--version 99` → 400.
9. **Restore 1 click**: thêm 2 destination (MinIO dev + MinIO thứ 2), Test connection pass/fail đúng. Backup now trên node pg14 → file hiện trong trình duyệt bucket → restore vào node mới pg17 → dữ liệu khớp. Thử restore file pg17 vào node pg14 → bị chặn với lý do rõ. Upload tay 1 file `.sql.gz` lên bucket → restore được sau khi chọn engine.
10. Xóa dải của máy test khỏi allowlist → kết nối 5432/3306 và Web UI bị từ chối; thêm lại → vào được.
11. Unit test: sinh file Traefik yaml (router + allowlist + cert), ký cert từ internal CA, đoán engine/version từ key S3, luật tương thích version khi restore, validate catalog, lệnh ProxySQL, quyền theo role; mỗi engine có 1 test tích hợp create→dump→restore (dùng Docker thật, chạy được bằng `go test -tags integration`).

## Sau khi duyệt

Copy tài liệu này vào `docs/superpowers/specs/2026-10-20-fridge-design.md` (git init repo), rồi chạy `superpowers:writing-plans` để ra plan triển khai từng bước.
