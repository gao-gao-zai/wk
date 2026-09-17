# 构建、部署与运维手册

面向**开发者**的构建与部署流程。面向使用者的快速启动（docker compose）见 [README.md](README.md)。

本文的部署目标是一个**通用的 Linux 服务器**（下文以 `srv` 代指，SSH 别名；实际示例中的 `007` 即此角色）。
要求：root 或 sudo 权限、Docker 已安装、服务器与本机之间 SSH 免密可通。

---

## 0. 前置条件（本机）

| 工具 | 用途 | 检查 |
|---|---|---|
| Go ≥ 1.23 | 交叉编译 Linux 二进制 | `go version` |
| Node 22 + pnpm | 构建控制台（react/） | `pnpm --version` |
| Docker Desktop（或同类） | 打镜像 | `docker version` |
| OpenSSH + 免密密钥 | 推送与远程操作 | `ssh srv 'echo ok'` |

SSH 免密示例（`~/.ssh/config`）：

```
Host 007
    HostName <服务器 IP>
    Port <端口>
    User root
    IdentityFile ~/.ssh/id_ed25519
```

> 下文 `ssh`/`scp` 命令里的 `007` 都指这个别名；部署到其它服务器时替换成你的别名即可。

---

## 1. 构建

### 1a. 标准 Docker 构建（网络正常时）

```bash
docker build -t workbuddy2api:latest .
```

`Dockerfile` 是三阶段构建：node 构建 react → golang 编译 → alpine 运行时。
需要拉取 `node:22-alpine`、`golang:1.23-alpine`、`alpine:3.20` 基础镜像，
**本机连不上 docker.io 时用 1b**。

### 1b. 离线构建（docker.io 不可达时的 rootfs 导出方案）

思路：从**服务器上已有的旧镜像**导出 rootfs 当基础层，`FROM scratch` 叠加，
再 COPY 本机交叉编译的二进制和前端产物——全程不需要访问 docker.io。

前置：服务器上已有旧版本镜像（首次部署走 1a 或让服务器自行 `docker pull alpine:3.20` 后 `docker save` 拉回来）。

```powershell
# Windows PowerShell，工作目录 = 仓库根

# 1) 交叉编译 Linux 二进制
$env:CGO_ENABLED='0'; $env:GOOS='linux'; $env:GOARCH='amd64'
go build -trimpath -ldflags='-s -w' -o .build-out\wb2api .\cmd\server
go build -trimpath -ldflags='-s -w' -o .build-out\login .\cmd\login

# 2) 构建前端（产物在 react/dist）
cd react
.\node_modules\.bin\vite.CMD build
cd ..
# 同步到 Go 服务引用的 frontend/ 目录
Remove-Item frontend\assets\* -Force
Copy-Item react\dist\assets\* frontend\assets\ -Force
Copy-Item react\dist\index.html frontend\index.html -Force

# 3) 从服务器旧镜像导 rootfs
ssh 007 'docker create --name wk-rootfs-tmp wk:linux-amd64 true'
ssh 007 'docker export wk-rootfs-tmp -o /tmp/wk-rootfs.tar && docker rm wk-rootfs-tmp'
scp 007:/tmp/wk-rootfs.tar .\wk-rootfs.tar
ssh 007 'rm -f /tmp/wk-rootfs.tar'

# 4) 拼镜像（Dockerfile.tmpbuild 内容见下方）
docker build -f Dockerfile.tmpbuild --tag workbuddy2api:latest .
Remove-Item .\wk-rootfs.tar, .\Dockerfile.tmpbuild -Force

# 5) 导出 tar 待推送
docker save workbuddy2api:latest -o .build-out\workbuddy2api-new.tar
```

`Dockerfile.tmpbuild` 内容（脚本生成，也手写同效）：

```dockerfile
FROM scratch
ADD wk-rootfs.tar /
USER root
RUN rm -f /app/config.json && chown -R app:app /app
USER app
WORKDIR /app
COPY .build-out/wb2api /app/wb2api
COPY .build-out/login /app/login
COPY frontend /app/frontend
ENV WB2A_LISTEN=:7863
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
```

> `RUN rm -f /app/config.json`：rootfs 来自旧容器镜像，里面可能残留上一版
> 打进去的配置文件；运行时配置以挂载卷为准，镜像里不能带。

### 1c. 前端构建的坑

- **不要用 `pnpm --dir react build`**：pnpm 的 supply-chain 检查会拦 esbuild
  的 postinstall（`ERR_PNPM_IGNORED_BUILDS`）。直接跑 vite 即可：
  `react\node_modules\.bin\vite.CMD build`（Linux/macOS 为 `./node_modules/.bin/vite build`）。
- esbuild 平台二进制在 `react/node_modules/@esbuild/win32-x64/`，即使
  `esbuild/esbuild.exe` 缺失（postinstall 没跑）vite 也能正常工作。
- 构建产物 `index-<hash>.js` 的 hash 变了就要同步 `frontend/`（Go 服务的
  静态资源目录），否则镜像里还是旧前端。

### 1d. 依赖更新

```powershell
cd react
pnpm install            # 需要 approve-builds 时按提示处理
```

Go 侧正常 `go mod tidy`。

---

## 2. 部署到 Linux 服务器

### 2a. 服务器侧目录约定（首次部署准备）

```bash
# 服务器上执行一次
mkdir -p /opt/wk
# 配置（属主与容器内 UID 一致，10001 是 Dockerfile 里 adduser 的值）
# 首次可从仓库 config.example.json 复制后编辑，api_key 必须是强随机值
chown -R 10001:10001 /opt/wk

# 需要的挂载内容：
#   /opt/wk/config.json     主配置
#   /opt/wk/auths/          账号授权文件目录
#   /opt/wk/data/           运行数据（state.json / metrics.db / 分组存储等）
#   /opt/wk/proxies.txt     短信直登代理列表（可选）
```

### 2b. 推送与替换容器

```bash
# 本机：推镜像
scp .build-out/workbuddy2api-new.tar 007:/tmp/workbuddy2api-new.tar

# 服务器：加载并替换（保留旧容器名便于回滚）
ssh 007 'bash -s' <<'EOF'
set -e
docker load -i /tmp/workbuddy2api-new.tar
rm -f /tmp/workbuddy2api-new.tar
docker tag workbuddy2api:latest wk:linux-amd64
docker rename wk-linux-amd64 wk-linux-amd64-prev      # 旧容器改名保留
docker stop -t 30 wk-linux-amd64-prev
docker rm wk-linux-amd64-prev                          # 确认新版本正常后再删
docker run -d --name wk-linux-amd64 \
  --restart unless-stopped \
  -e TZ=Asia/Shanghai \
  -p 127.0.0.1:7863:7863 \
  -v /opt/wk/proxies.txt:/opt/wk/proxies.txt:ro \
  -v /opt/wk/auths:/app/auths \
  -v /opt/wk/data:/app/data \
  -v /opt/wk/config.json:/app/config.json \
  wk:linux-amd64
sleep 4
# 若有 docker 网络别名（供同宿主机其它容器用域名访问，如 new-api 网关）：
docker network connect --alias wk new-api-net wk-linux-amd64 || true
EOF
```

要点：
- `-p 127.0.0.1:7863:7863` 只绑回环，外部访问走反代或 docker 网络，**不要**直接暴露公网。
- `-v /opt/wk/data:/app/data` 是**必须**：state.json、metrics.db、分组/密钥存储都在里面，不挂载重启即丢。
- `TZ=Asia/Shanghai` 影响签到时刻（次日 04:00 硬冷却等）与日志时间戳。

### 2c. 验证

```bash
# 容器健康（健康检查内建，Up x minutes (healthy)）
ssh 007 'docker ps --filter name=wk-linux-amd64 --format "{{.ID}} {{.Status}}"'

# 服务健康：期望 {"healthy":N,"total":N}
ssh 007 'curl -s http://127.0.0.1:7863/healthz'

# 若接了 docker 网络别名，从网关容器验证：
ssh 007 'docker exec new-api wget -qO- --timeout=5 http://wk:7863/healthz'

# 前端版本：返回的 HTML 里 bundle hash 应等于本次构建的 index-<hash>.js
ssh 007 'curl -s http://127.0.0.1:7863/ | grep -o "index-[A-Za-z0-9_-]*\.js"'

# 业务闭环：发一个真实请求，看请求日志的 credits 与 credit_source
KEY=$(ssh 007 'grep -o "\"api_key\"[^,]*" /opt/wk/config.json | cut -d\" -f4')
ssh 007 "curl -s -X POST http://127.0.0.1:7863/v1/chat/completions \
  -H 'Authorization: Bearer $KEY' -H 'Content-Type: application/json' \
  -d '{\"model\":\"auto\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":10}'"
```

### 2d. 回滚

替换脚本在 `docker rm wk-linux-amd64-prev` **之前**都保留了旧容器；
新版本有问题时：

```bash
ssh 007 'docker stop wk-linux-amd64 && docker rm wk-linux-amd64 && \
  docker rename wk-linux-amd64-prev wk-linux-amd64 && docker start wk-linux-amd64'
```

若旧容器已删但旧镜像 tag 还在：`docker tag <旧imageID> wk:linux-amd64` 后重跑 2b 的 run。

---

## 3. 测试与提交约定

```bash
go build ./...
go test ./...
go vet ./...
gofmt -l .        # 应为空
```

- **提交只在明确说"提交"时进行**；提交信息中文、说明动机而非流水账。
- 默认**不推送**远端（`git push` 需明确要求）。
- `tools/` 下的分析脚本不提交（个人运维用）。

---

## 4. 常见问题

**Q: 服务器磁盘上 metrics.db 越来越大？**
请求日志保留策略默认 1 万条；控制台「管理设置 → 请求日志保留策略」可改为
天数+条数双条件（例：30 天 + 20 万条），即时生效。

**Q: 新镜像里前端没更新？**
构建前端后忘记同步 `frontend/`（见 1b 第 2 步），或浏览器缓存了旧
`index.html`——强刷（Ctrl+F5）。

**Q: `docker load` 后容器起不来？**
先看日志 `docker logs wk-linux-amd64`；最常见的两类：
配置文件属主不对（应为 UID 10001，见 2a）和 `data/` 目录没挂载导致
state.json 冲突。

**Q: 怎么加新服务器？**
`~/.ssh/config` 加别名（见第 0 节），服务器上跑一遍 2a 的目录准备，
推送一个基础镜像（1a 构建后 `docker save | ssh ... docker load`），
之后按 2b 走。
