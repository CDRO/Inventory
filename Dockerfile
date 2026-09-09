# Single self-contained build: anything that builds locally deploys unchanged
# to the NAS. Nothing is installed on the host.
# See docs/specs/01-architecture-and-deployment.md.

# ---- builder ----
FROM golang:1-alpine AS builder
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/inventory ./cmd/inventory

# ---- dev stage (used by docker-compose.override.yml) ----
FROM golang:1-alpine AS dev
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
CMD ["go", "run", "./cmd/inventory", "serve"]

# ---- production ----
# scratch carries no CA bundle and no timezone database. Both are copied from
# the builder: without the certificates every outbound HTTPS call (Gemini,
# SerpAPI, Iconify) fails certificate verification, and without zoneinfo the
# expiry-date arithmetic in docs/specs/08-expiration-and-classification.md has
# no timezone data to work from.
FROM scratch AS prod
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
# `migrate up` runs from this same image, so the goose SQL files have to travel
# with the binary; there is no host toolchain to apply them from.
COPY --from=builder /src/migrations /migrations
COPY --from=builder /out/inventory /inventory
EXPOSE 8000
ENTRYPOINT ["/inventory"]
CMD ["serve"]
