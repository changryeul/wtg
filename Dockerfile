# WTG — 모든 Go 서비스가 담긴 단일 이미지.
#
# multi-stage:
#   builder  — golang:1.25-alpine + make → build/bin/{mci-*, quote-forwarder, ...}
#   runtime  — alpine:3.20 (tzdata + ca-certificates) — shell 있어 debug 편리
#
# compose 에서 서비스별로 command 지정:
#   command: ["mci-price", "--listen", ":8082", ...]
# PATH 에 /app/bin 등록되어 있어 short name 가능.
#
# 이미지 tag 는 CI 가 <git-sha> + latest 로 GHCR 에 push.
#   ghcr.io/changryeul/wtg:<sha>
#   ghcr.io/changryeul/wtg:latest

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache make git bash

WORKDIR /src

# go mod 캐시 최적화 — 소스 변경 없이 deps 만 바뀌면 여기까지 캐시.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO 비활성 (static binary), Linux amd64 명시.
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN make build

# ────────────────────────────────────────────────────────────────

FROM alpine:3.20

RUN apk add --no-cache tzdata ca-certificates && \
    ln -sf /usr/share/zoneinfo/Asia/Seoul /etc/localtime

WORKDIR /app

# 바이너리 + 기본 카탈로그 파일.
COPY --from=builder /src/build/bin/ /app/bin/
COPY --from=builder /src/etc/ /app/etc/

ENV PATH=/app/bin:$PATH
ENV TZ=Asia/Seoul

# default entrypoint 는 지정 안 함 — compose 에서 command 로 서비스 선택.
# 이미지 크기 최소 ~150MB (alpine + Go binaries).
