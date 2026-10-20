FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /fridge ./cmd/fridge

# Runs as root: it needs /var/run/docker.sock.
FROM gcr.io/distroless/static
COPY --from=build /fridge /fridge
ENTRYPOINT ["/fridge", "server"]
