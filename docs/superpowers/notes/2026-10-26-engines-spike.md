# Spike: mTLS isolation for Redis, MongoDB, MySQL and MariaDB, and a MySQL router

`scripts/swarm-test/spike-engines.sh [redis|mongo|mysql|mariadb]` runs each engine in the dind swarm with the node's own client
certificate as the only trusted client root, and tries clients with the right certificate, another certificate, none, plain
text and a wrong password. Every engine accepted exactly the right client and refused the others at the TLS handshake:

| engine | server side | notes |
|---|---|---|
| Redis 7.4 | `tls-port`, `port 0`, `tls-auth-clients yes`, `tls-ca-cert-file` = the node's client cert, `aclfile` | users either in redis.conf or in the ACL file, never both (it refuses to start); the file is written on first start only, so `ACL SAVE` after a password rotation survives restarts; `redis-cli` exits 0 on a refused command; `redis-cli --rdb -` streams a dump |
| MongoDB 7.0 | `--tlsMode requireTLS --tlsCertificateKeyFile (cert+key) --tlsCAFile <client cert> --auth`, root via `MONGO_INITDB_ROOT_*` | the official entrypoint's init phase copes with `requireTLS`; `mongodump` takes TLS options only through `--uri` (`tls=true&tlsCertificateKeyFile=…&tlsCAFile=…`), not `--tls` |
| MySQL 8.4 | `--ssl-ca` = client cert, `--require-secure-transport=ON`, `--mysql-native-password=ON`, users `IDENTIFIED WITH mysql_native_password … REQUIRE X509` (root too, afterwards) | 8.4 disables the native plugin unless asked |
| MariaDB 11.4 | same flags, users `IDENTIFIED BY '…' REQUIRE X509` | `IDENTIFIED WITH mysql_native_password` is a syntax error here; `mariadb-dump` instead of `mysqldump` |

## The router (`internal/mysqlproxy`, `fridge mysql-router`)
MySQL speaks first, so there is no SNI. The router sends a made-up greeting (capabilities limited to what every server accepts),
reads the user name from the handshake response, dials that user's database over TLS with the node's client certificate,
and then lets the client prove its password against the database's own scramble: when the database's default plugin is not
native (MySQL 8) it answers the first response with an AuthSwitchRequest, which the router turns into one challenge for the
client; when it is native (MariaDB) the greeting's scramble is the challenge. After the OK it copies bytes.

Verified against MySQL 8.4 and MariaDB 11.4 with both the `mysql` and the `mariadb` command-line clients: login, queries,
a 200 KB result, wrong password, unknown user, plain text (refused: TLS is required).

Found on the way:
- the backend handshake must be masked to the capabilities the made-up greeting offered; passing the client's own flags through
  made MySQL 8.4 expect query attributes and answer every query with "Malformed communication packet" (error 1835);
- with TLS 1.3 a database refusing the router's client certificate shows up on the first read, not in the handshake;
- the `mariadb` client verifies the host name as soon as `--ssl-ca` is given, even with `--skip-ssl-verify-server-cert`.
