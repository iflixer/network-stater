# ---------- 1) build stage ----------
FROM golang:1.23-alpine AS builder
WORKDIR /src

# Если нужны приватные модули — добавь SSH/токены тут
RUN apk add --no-cache git ca-certificates
COPY src/go.mod ./
RUN go mod download
COPY src/ .
# Сборка статически линкованного бинаря (чтобы он шёл в distroless:static)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -trimpath -ldflags="-s -w" -o /out/network-stater .

# ---------- 2) базовый слой с certs для копирования ----------
FROM alpine:3.20 AS certs
RUN apk add --no-cache ca-certificates

# ---------- 3) PROD: distroless (без shell) ----------
FROM gcr.io/distroless/static:nonroot AS prod
# distroless обычно пустой — кладём корневые сертификаты
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/network-stater /usr/bin/network-stater
USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/network-stater"]

# ---------- 4) DEBUG: alpine (есть sh, busybox, strace и пр. по желанию) ----------
FROM alpine:3.20 AS debug
RUN apk add --no-cache ca-certificates bash busybox-extras curl net-tools iproute2 bind-tools strace
COPY --from=builder /out/network-stater /usr/bin/network-stater
# В дебаг-образе оставим root для удобства
ENTRYPOINT ["/usr/bin/network-stater"]
