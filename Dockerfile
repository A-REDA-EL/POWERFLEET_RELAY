# --- UI ---
FROM node:24-alpine AS ui
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# --- Go binary (UI embedded) ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web/embed.go ./web/embed.go
COPY --from=ui /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/relay ./cmd/relay \
 && mkdir -p /out/data

# --- runtime: no shell, non-root ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/relay /relay
COPY --from=build --chown=65532:65532 /out/data /data
ENV RELAY_DATA_DIR=/data RELAY_LISTEN=:8090
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/relay", "healthcheck"]
ENTRYPOINT ["/relay"]
