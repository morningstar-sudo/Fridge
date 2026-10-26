# Two images from one build:
#   docker build -f deploy/Dockerfile --target job -t fridge-job .   # dump/restore/check jobs: Fridge + the clients of every engine
#   docker build -f deploy/Dockerfile -t fridge .                    # the server (the default, last stage)
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /fridge ./cmd/fridge

# The job image carries the client tools of every engine and version the catalog offers (pg_dump must match the server it
# dumps; the mysql and mongo tools talk to every version of their server). Jobs run as an unprivileged user, except the
# one that preloads a volume (it is told to run as root) and none ever sees the Docker socket.
FROM debian:bookworm-slim AS job
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
 && install -d /usr/share/keyrings /usr/share/postgresql-common/pgdg \
 && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
 && echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" > /etc/apt/sources.list.d/pgdg.list \
 && curl -fsSL https://repo.mysql.com/RPM-GPG-KEY-mysql-2025 | gpg --dearmor -o /usr/share/keyrings/mysql.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/mysql.gpg] http://repo.mysql.com/apt/debian bookworm mysql-8.4-lts" > /etc/apt/sources.list.d/mysql.list \
 && curl -fsSL https://www.mongodb.org/static/pgp/server-8.0.asc | gpg --dearmor -o /usr/share/keyrings/mongodb.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/mongodb.gpg] https://repo.mongodb.org/apt/debian bookworm/mongodb-org/8.0 main" > /etc/apt/sources.list.d/mongodb.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      postgresql-client-13 postgresql-client-14 postgresql-client-15 postgresql-client-16 postgresql-client-17 postgresql-client-18 \
      mariadb-client redis-tools mongodb-mongosh mongodb-database-tools \
 # Oracle's mysql client conflicts with the MariaDB one as a package, so it is unpacked beside it; /usr/local/bin comes first in PATH
 && mkdir /tmp/mysql && cd /tmp/mysql \
 && apt-get download mysql-community-client mysql-community-client-core mysql-community-client-plugins \
 && for f in *.deb; do dpkg -x "$f" /opt/mysql; done \
 && ln -s /opt/mysql/usr/bin/mysql /usr/local/bin/mysql && ln -s /opt/mysql/usr/bin/mysqldump /usr/local/bin/mysqldump \
 && cd / && rm -rf /tmp/mysql \
 && ! ldd /usr/local/bin/mysql /usr/local/bin/mysqldump | grep "not found" \
 && apt-get purge -y curl gnupg && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*
COPY --from=build /fridge /fridge
USER 65532:65532
ENTRYPOINT ["/fridge"]

# Runs as root: the server manages the swarm through the Docker socket.
FROM gcr.io/distroless/static AS server
COPY --from=build /fridge /fridge
ENTRYPOINT ["/fridge", "server"]
