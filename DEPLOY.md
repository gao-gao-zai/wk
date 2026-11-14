# 构建、部署与运维手册

> **2026-11-14 生效：dogyun 已完成首次迁移，常规业务升级统一使用 overlap。** 先让候选接新推理请求，再让旧 HTTP/SSE 自然结束，最后交接管理与后台。不能再先 hold 推理入口、等旧请求或整个后台批次排空后才切流。
>
> 项目执行约定见 [AGENTS.md](AGENTS.md)，本次故障与恢复证据见 [运维复盘](docs/operations/2026-11-14-overlap-deployment.md)。旧的 `007`、本机/容器内编译、停旧再开新、直接启动旧 serial 回滚等示例已撤下。首次迁移不属于后续常规发布步骤。

## 1. 当前生产定位

以下是 2026-11-14 验收快照；每次发布先读线上指针、私有控制状态及字段白名单，不能盲用历史 release/镜像/端口。

| 对象 | 当前定位 |
| --- | --- |
| 构建/测试 | 本项目 `.build-test.json` 指定 SSH **`tyy2`** |
| 生产 / 公网 | SSH **`dogyun`** / `https://wb.gaozaiya.cc` |
| 发布指针 | `/opt/wk/current-release.json`、`/etc/wb2api-upgrade/current.json` |
| 活动业务 | `wk-release-growth-current-routefix-20261114`，回环端口 `7864` |
| 稳定 ingress | `wk-upgrade-ingress`；发布端口 `7863`，socat 转发到共享网络命名空间的 `127.0.0.1:7866` |
| 稳定 gateway | `wk-handoff-gateway-overlap-20261114`，带 `-overlap`；控制 socket `/run/wb2api-upgrade/gateway-overlap.sock` |
| gateway 状态文件 | 宿主 `/opt/wb2api-upgrade/gateway-overlap.state` |
| 业务控制 socket | 宿主 `/run/wb2api-upgrade/<release>.sock`；容器 `/upgrade-run/<release>.sock` |
| writer 锁 | 宿主 `/run/wb2api-upgrade/writer.lock`；容器 `/upgrade-run/writer.lock` |
| 当前协调目录 | 宿主 `/opt/wk/data/upgrade-members-routefix-20261114`；容器 `/app/data/upgrade-members-routefix-20261114` |
| 当前协调域 | `70d230270258628b270a340fd32b5aa7` |
| 持久挂载 | `/opt/wk/config.json → /app/config.json`、`/opt/wk/auths → /app/auths`、`/opt/wk/data → /app/data`、`/run/wb2api-upgrade → /upgrade-run` |
| 代理文件 | 保留 `/app/proxies.txt` 与 `/opt/wk/proxies.txt` 两个已有只读挂载 |
| 监督单元 | `wb2api-current.service`、`wb2api-handoff.service`、`wb2api-ingress.service` |

业务和 gateway 与 ingress 共用网络命名空间。控制器检查候选的回环 HTTP 时也必须进入这个命名空间；不要把宿主的 `127.0.0.1:7864` 当作同一个监听。保留当前 `new-api-net`、network aliases 和实际挂载，不重新发明拓扑。

旧容器与镜像保留，但 Docker restart policy 为 `no`；systemd 只监督当前业务版本，`Restart=on-failure`。正常业务发布不重建/重启稳定 ingress、gateway 或 PostgreSQL；尤其不能随意 stop/restart 被当前业务单元依赖的 handoff 单元。

## 2. 发布前核对、构建与备份

1. 核对当前业务和 gateway 的 `mode=overlap`、协议 1、release 一致、无 held/transition/recovery_required；当前业务 ready 且为唯一后台 owner。检查当前 coordination domain 和配置中的协调目录。条件不成立就保留当前入口，报告具体原因，不能自动降级 serial。
2. 从实际运行实例复制必要配置、环境、全部挂载、UID、网络及资源限制。候选独立 release、端口和 control socket，但使用**完全相同的 writer 锁、协调目录、域、配置/凭据/数据/数据库和 session salt**。日常升级不创建新 domain，不执行 reference 重绑定。
3. 在 `tyy2` 隔离源码目录纳入此次实际要发布的未提交及未跟踪源码；保留 revision、dirty、逐文件 SHA256。先检查 SSH 与数据盘，再 source 项目 env。旧资料写 `tdog` 时以本项目配置为准。

```bash
# 连接预检；不在本机或生产机编译
ssh -o BatchMode=yes -o ConnectTimeout=12 -o StrictHostKeyChecking=yes tyy2 true

# 以下在 tyy2 隔离任务中执行
/usr/local/bin/build-storage-check
source /srv/build-server/env/workbuddy2api.sh
```

4. 使用当前宿主机构建脚本/经核对的发布任务生成 binary、前端、完整 `source.tar.gz`、LICENSE、NOTICE、LICENSES。Docker 只复制宿主机产物。前端构建的 `--emptyOutDir` 会清空旧资源：应在源码归档及 Go 编译之前合入上一版仍会被旧页面引用的带 hash 资源，不能只保留新 index。归档与实际 binary 必须来自同一最终源码快照，排除生产配置、凭据和业务数据。
5. 按变更运行 Go 回归，真实临时 PG 的 pool/coordination、必要 race，以及 `test-upgrade-controller.py`、`test-seamless-upgrade.py`、`test-overlap-server.py`。涉及共享 pool/采集/启动路径时，加 41 级别假账号、ready 后**立即** 4 路 SSE + health/status、真实 PG Routes/Leases/释放及至少 180 秒覆盖；不先预热 `/status`，不延长原超时掩盖问题。测试 DSN 仅指向本机临时库。
6. 校验完整产物和传输 SHA256，记录字节数、构建主机和不可变 image ID。`dogyun` 使用 `scp -O`；Windows 不用文本管道传二进制。增量包只能在断言 base image ID 后 COPY 新 binary、配套 source 和许可文件；不现场编译，不顺带替换未变 gateway。
7. 写入生产前在线备份 PG、必要 SQLite、config/auths/data、入口/容器/systemd/发布元数据。备份加密，验证可解密及格式可读；密钥使用既有受控机制，不写明文。记录当次账号集合摘要、凭据文档可读性/数量、关键状态及当前权威域；41 是本次历史值，下次应从线上取得基线。

只输出脱敏字段，不打印完整配置、环境、DSN、认证头、私钥或生产日志。认证在执行进程内从现有配置/凭据机制注入。普通验收使用只读状态与缓存快照，真实上游生成/登录测试须有相应授权。

## 3. 候选准备与常规 overlap 切换

先创建停止状态的候选，以及独立的只读 `-preflight` 容器。预检容器与候选使用相同环境、镜像及挂载位置；预检不代表候选已经可以接流。候选必须使用：

```text
-upgrade-mode overlap -coordination-dir /app/data/upgrade-members-routefix-20261114
-state-lock /upgrade-run/writer.lock -release <new-release> -control /upgrade-run/<new-release>.sock
```

上述目录为当前生产值，创建候选前需与当前实例核对一致。候选启动后由现有 owner 约束为 standby，不得提前争抢后台。

每次准备独立发布 wrapper，在 `/run/wb2api-upgrade/deployment.lock` 下覆盖预检、交接、失败处理及指针/监督器提交；监控用**独立 systemd 单元**，有结束条件和运行时限。辅助进程继承守卫，超时要终止并等待整个辅助进程组，防止孤儿继续执行；监控不能随部署单元失败而退出。

以下命令是 wrapper 内的控制器调用结构；变量来自该次已核实的容器/指针及独立 journal，不包含秘密：

```bash
# CLI 为兼容首次迁移仍默认 serial，必须显式传 --mode overlap。
INGRESS_PID=$(docker inspect --format '{{.State.Pid}}' wk-upgrade-ingress)
nsenter -t "$INGRESS_PID" -n python3 "$CONTROLLER" --mode overlap \
  --old-container "$OLD_CONTAINER" --new-container "$NEW_CONTAINER" \
  --check-container "$PREFLIGHT_CONTAINER" \
  --old-release "$OLD_RELEASE" --new-release "$NEW_RELEASE" \
  --old-control "$OLD_CONTROL" --new-control "$NEW_CONTROL" \
  --gateway-control /run/wb2api-upgrade/gateway-overlap.sock \
  --new-target "$NEW_LOOPBACK_ORIGIN" \
  --drain-seconds "$DRAIN_SECONDS" --start-seconds "$START_SECONDS" \
  --journal "$NEW_JOURNAL"
```

`CONTROLLER` 使用此次审阅过的 `scripts/seamless-upgrade.py`，不是历史 `switch_service.py`、`switch_retry.py` 或 `seamless-upgrade-rebind.py`。旧 journal 存在时必须先调查已提交阶段，不能删除后重跑。

控制器顺序为：

1. 核对镜像、挂载、同 writer/协调目录及重启策略，运行只读预检。
2. 启动候选 standby；校验 release、ready、`mode=overlap`、协议、**与旧版本相同的域**、候选尚非后台 owner，并实际检查候选回环 `/readyz`。
3. 暂停旧后台新增工作，立即 `/switch-live` 把后续推理交给候选。**不先等旧请求或已接收的后台工作归零，不 hold 新推理。**
4. 旧版本继续完成已准入 HTTP/SSE 与已接收后台工作。管理读取仍在旧 owner；重叠期间管理写入受保护。
5. 旧推理/后台结束后，仅短暂 hold 管理入口，正常 stop 旧版本并确认真实退出；候选接管管理/后台，核对同域、owner、ready，再切管理并退役旧路由。
6. 原子更新 `/etc/wb2api-upgrade/current.json`、活动 systemd 单元与发布指针，只监督新业务版本；旧容器保持 restart=no。记录 coordination directory/domain 与下次 `upgrade_mode=overlap`。

排空是**切新请求之后**的旧实例退役条件，不是升级开始前的全站等待。无限长流不能靠强杀来完成发布；超预算按控制器回执撤销或进入人工调查，不能回到“serial 等旧流”的套路。

当前 gateway wait=90 秒。脚本要求管理预算至少 `drain-seconds + 2 × start-seconds + 15`；例如 30/20/90 满足条件，但不是所有流时长都适用。根据实际流时长制定预算；扩大预算也需保持稳定入口，不能临场重启 gateway 打断流。只读检查和有限等待不保证所有客户端链路零超时。

## 4. 验收、失败与恢复边界

- 持续采样本机入口与公网的 `/healthz`、`/readyz`，先记录 HTTP 状态再解析；记录 release、耗时和脱敏网络错误。用私有控制 socket 确认 gateway/业务同域、同 release、唯一后台 owner、无 transition/recovery_required。
- 比对当次账号集合、凭据文档数量/可读性和关键状态延续；核对 PG 未被替换、恢复或重启，原持久记录保留。检查新域中真实 Routes/Leases 及正确释放。已有请求自然完成，不能仅用“新健康接口 200”证明流未中断。
- 连续观察至少 300 秒，覆盖启动及后续定时周期；核对前端 index 和当前/上一版静态资源、配套 `/legal/source` SHA256、鉴权、缓存任务快照、三监督单元与重启策略。结束临时监控，保存实际失败样本、发布清单与回执；不能把已知错误省略成零中断。
- `aborted_old_serving`：确认实际已回到旧 overlap、候选工作收尾后，以新 journal 重试。不要假定所有失败都能自动恢复。
- `recovery_required`、切流/stop 回执不明、计数未知：先读控制状态、容器 State 和阶段证据，保护已知入口；不盲目重复启动/停止，不重放用户请求，不清零活动数，不强杀长流或恢复整库。
- 新共享域一旦 bootstrap，后台可能在 ready 前已经写入；共享 flush 不导回 legacy serial 存储。**禁止直接启动旧 serial，或切回串行运行前的过期共享域。** 兼容回滚使用当前域的 overlap 协议；不兼容 schema/协议另行处理，保留当前数据。
- 状态页显示零账号时先判断共享池是否 fence，核对 PG 与凭据文件；不能据此清空池、覆盖配置、换域或导入备份。
- 保留旧镜像/容器和加密备份，但“旧容器存在”不是安全回滚证明。包含旧 NUL 路由实现的业务 binary 不可作为 overlap 回滚版本。

## 5. 提交与日常运维

提交仅在用户明确要求时进行，默认不 push；保留其他未提交修改。文档更新不触发编译或部署。构建主机选择、授权范围和密钥边界依项目及工作区约定。

日志体积通过控制台请求日志保留策略管理，不在部署中顺手删数据库；前端版本按资源 SHA256 校验。部署排障只提取所需状态/错误分类，不原样输出生产日志或通过命令行带出 API key。
