# syntax=docker/dockerfile:1

# ---------- 构建阶段：生成 protobuf 代码并编译 ----------
FROM golang:1.25-bookworm AS build

ENV GOPROXY=https://proxy.golang.org,direct \
    GOSUMDB=off \
    CGO_ENABLED=0

RUN apt-get update \
 && apt-get install -y --no-install-recommends protobuf-compiler libprotobuf-dev ca-certificates \
 && rm -rf /var/lib/apt/lists/*

RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest \
 && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

WORKDIR /src
COPY . .

# 由 proto 生成 Go 代码 -> 整理依赖 -> 编译 server 与 cli
RUN protoc --go_out=. --go_opt=module=schoolbus \
           --go-grpc_out=. --go-grpc_opt=module=schoolbus \
           proto/schoolbus/v1/schoolbus.proto \
 && go mod tidy \
 && go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && go build -trimpath -ldflags="-s -w" -o /out/cli ./cmd/cli

# ---------- 运行阶段：非 root 用户 + HEALTHCHECK ----------
FROM alpine:3.20

RUN addgroup -S app && adduser -S app -G app
WORKDIR /app

COPY --from=build /out/server /app/server
COPY --from=build /out/cli /app/cli

USER app
EXPOSE 8080 9090

HEALTHCHECK --interval=15s --timeout=3s --start-period=25s --retries=5 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

CMD ["/app/server"]
