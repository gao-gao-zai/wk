# syntax 指令已移除：需要访问 docker.io 拉 dockerfile frontend，离线环境会
# 卡在第一步；本文件未使用任何 BuildKit 扩展语法，默认 frontend 即可。
FROM node:22-alpine AS frontend-build
WORKDIR /src/react
COPY react/package.json react/package-lock.json ./
RUN npm ci
COPY react/ ./
RUN npm run build

# go.mod 要求 go >= 1.26；docker.io 不可达时走 daocloud 镜像源拉取本镜像
FROM docker.m.daocloud.io/library/golang:1.26-alpine AS build
WORKDIR /src
# 离线环境：模块代理走 goproxy.cn（境内可达），docker.io 走 daocloud 已在 FROM 解决
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
# 容器内必须 bind 所有网卡端口映射才生效；默认配置只监听回环。
ENV WB2A_LISTEN=:7863
USER app
WORKDIR /app
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/login /app/login
COPY --from=frontend-build /src/react/dist /app/frontend
# 不再预置 config.json。以前这里 COPY config.example.json，等于把一个公开仓库里的
# 占位符当作线上凭据发布；缺配置时改走 env（WB2A_API_KEY 等），且
# requireRealCredential 会拒绝占位符或空凭据启动。
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
