# Two images from one build:
#   docker build -f deploy/Dockerfile --target job -t fridge-job .   # dump/restore/check jobs: Fridge + PostgreSQL clients 13-18
#   docker build -f deploy/Dockerfile -t fridge .                    # the server (the default, last stage)
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /fridge ./cmd/fridge

# The job image carries every client version the catalog offers (pg_dump must match the server it dumps).
# Jobs never need root and never see the Docker socket.
FROM debian:bookworm-slim AS job
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
 && install -d /usr/share/postgresql-common/pgdg \
 && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
 && echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" > /etc/apt/sources.list.d/pgdg.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends postgresql-client-13 postgresql-client-14 postgresql-client-15 postgresql-client-16 postgresql-client-17 postgresql-client-18 \
 && apt-get purge -y curl gnupg && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*
COPY --from=build /fridge /fridge
USER 65532:65532
ENTRYPOINT ["/fridge"]

# Runs as root: the server manages the swarm through the Docker socket.
FROM gcr.io/distroless/static AS server
COPY --from=build /fridge /fridge
ENTRYPOINT ["/fridge", "server"]
