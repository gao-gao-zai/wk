

## 快速开始

DeepSeek 请求对齐参考实现的思考形式：未指定开关时补 `thinking.type=enabled`，未指定档位时以 `high` 兜底，再按账号已知能力降级；显式 `disabled` 保持关闭并移除平铺思考档位。开启思考或历史存在推理痕迹时，assistant 历史补齐字符串 `reasoning_content` 和非空 `reasoning`，保留已有文本，缺失值使用空格占位。其他模型不启用这些规则。这是协议兼容修复，尚不证明 11140 限制的触发原因。

### 错误请求自动捕获

“请求日志”页的“错误请求自动捕获”默认启用，捕获业务错误码 `11128`、`11140`、`11148`，可在该卡片修改错误码或暂停。匹配发生在每一次上游尝试；即使随后换号重试成功、最终 HTTP 200，也会保留此前的失败尝试。它不改变请求、重试或账号状态，也不会额外向供应商发请求。

上游 HTTP 错误明确报告账号受限时，该账号仍立即禁用，同时结束当次请求，保留上游错误状态并返回 `upstream_account_restricted`，不把相同正文换号重放。这是限制扩散的保护，不代表已找到首次限制的触发原因，也不会取消其他已在途的请求。

Chat 的 `max_completion_tokens` 有效正整数会转换为上游使用的 `max_tokens`，显式的 `max_tokens` 优先（包括 null）；无效或未设置的别名不添加输出上限。Responses 的 `max_output_tokens` 继续沿用原有转换。

记录包含 WorkBuddy 收到的入口请求、转换后的 Chat 请求、每个账号实际发往上游的请求和错误、请求 ID，以及账号/代理的稳定脱敏标识。WorkBuddy 入口位于 NewAPI 之后，因此无法还原 NewAPI 转换前的客户端请求。SSE 中途出现业务错误同样可捕获。

记录只在内存中供管理员查看，不写常规请求日志、业务数据库、文件或浏览器存储；保留10分钟，最多20条、总计32 MiB，重启即丢失。每份记录的请求正文预算为6 MiB，单份正文上限2 MiB；超限仅保存大小和哈希并标明省略。最多同时采样8个在途请求，繁忙时跳过数会显示在界面。每请求至多记录8次上游尝试，超出部分计数标明省略。常规成功请求不留存。

抓取只保留白名单元数据头，不记录认证头、Cookie 或原始账号 UID。正文中的已知认证值、常见密钥格式、带标签的密码/令牌、JSON 字符串内嵌凭据会脱敏，内联图片数据会省略；自动脱敏无法识别所有无标签的私密文本，记录仍按敏感请求内容处理。原始正文 SHA-256 在脱敏之前计算，用于对照；`hashed_bytes` 标明哈希覆盖的实际字节数，`complete`/`truncated` 标明不完整内容。详情仅供受保护的管理员会话或全局管理员密钥访问，分组密钥不可读取。

管理员 API（均为本地观测，不触发上游）：`GET /admin/request-capture` 返回设置和记录摘要；`GET /admin/request-capture/{id}` 读取一份详情；`POST /admin/request-capture` 接收 `{"enabled":true,"business_codes":[11128,11140,11148]}`；`DELETE /admin/request-capture` 清空记录。设置仅本次进程生效，重启恢复默认。修改设置、暂停或清空会丢弃尚未完成的采样，实际推理请求继续。启用该功能不代表能阻止上游限制，也不证明捕获到的请求就是风控唯一原因。

需要对照成功与失败请求时，可向同一管理员接口 POST `{"structure_sample_seconds":600}`，开启限时结构采样（默认关闭，范围 0–1200 秒，0 为停止）。GET 返回的 `structures` 包含入口、标准 Chat 和实际出站三个阶段的大小/哈希、消息角色计数、工具定义大小/哈希、工具历史配对计数、assistant 推理字段类型及白名单协议控制值。不保留正文、工具名称/定义/参数/结果、任意字段名、认证头或原始工具调用 ID。正文超限或不完整时只提供大小/哈希并标记未分析；消息配对统计只作诊断，不修改或阻止请求。结构记录使用独立内存队列，最多 64 条、每条 32 KiB，保留 10 分钟，不占用或挤出失败正文记录。采样到期不再收集，已有结构按保留期自动释放；DELETE 同时清空结构并关闭采样。错误捕获暂停时仍可单独启用结构采样。采样状态可从 `structure_sampling` 和 `structure_sampling_until` 核对。

本修改版采用 **AGPL-3.0-only**。基于 / 包含了 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)、[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) 和 [workbuddy2api-panel](https://github.com/yreidev/workbuddy2api-panel) 的代码或材料；这是独立维护的修改版，不代表原作者或腾讯官方。版权与第三方授权见 [NOTICE](NOTICE) 和 [LICENSES](LICENSES)。

### 上游读取兼容

Chat 消息中连续的 assistant 工具调用会在结果配对前合并，保留文本、推理与工具上下文；不会跨越其它角色、纯文本 assistant 或冲突的消息元数据。上游 `11128` 的请求/内容拒绝不累计账号风控；`11140` 若明确是模型提供方限流，按软限流处理；明确的账号异常活动限制仍按账号受限处理。这些修复不等于已证明真实账号限制的触发原因。

账号能力与认证补充：模型思考档位按区域、UID、企业及账号类型隔离缓存，只在账号选定后应用，未知或过期能力保持透传。OAuth 与短信登录保留上游实际返回的账号类型、OneID、身份提供方、部门和令牌类型；重新登录不沿用旧授权的身份字段，设备档案继续保留。错误对象增加独立业务码，兼容数字及字符串形式，已知错误沿用现有分类，未知码不引入新的换号或冷却规则。

启动配置（修改后重启生效）：

- `upstream.endpoint_profile`：`legacy`（默认，沿用现有入口）或 `product`（国内业务 API 默认使用 `https://www.workbuddy.cn`）。可分别覆盖 `chat_base_cn`、`billing_base_cn`、`chat_base_global`、`billing_base_global`、`web_base_cn`。OAuth 登录工具复用所选 API 入口；短信 OneID/Keycloak 授权链保持独立，不随产品入口盲目迁移。
- `upstream.web_auth_mode`：`auto`（服务端默认）、`cookie` 或 `bearer`。Cookie 模式按账号、企业、区域、凭证和入口隔离内存登录态，使用一次性登录码建立会话，全程复用该账号的代理路由；不持久化 Cookie 或登录码。`auto` 仅在新认证接口不支持、响应不兼容或缺少必要凭证时回退旧 Bearer，认证失败、限流及网络错误不触发认证方式切换。未配置本组选项的库调用仍保持旧行为。
- Cookie 请求明确返回 401 时，最多更新登录态并重试一次，保持同一请求体和幂等键；超时、连接错误及其他失败不自动重放业务请求。成长接口的接受任务路径及部分空正文按 Cookie 模式适配；旧 Bearer 模式保留原请求。
- `upstream.reasoning_summary_auto`：服务端默认 `true`。仅当请求显式带字符串类型的思考档位，且没有设置 `reasoning_summary` 时补 `auto`；显式设置（包括 null）原样保留，可关闭，不增加长期缓存字段。
- 专家对话仅根据所选市场条目附加团队与云端/付费保护字段；仅有专家 ID 时不猜测这些属性。兼容统计及任务事件的执行逻辑不在本轮改动范围内。

- 凭证导入同时接受秒和毫秒格式的 `expiresAt`，内部及写回仍使用 Unix 秒。
- 专家列表优先读取 `items`，缺失或为 null 时兼容 `experts`；显式空数组保持为空。
- 个人余额优先读取资源汇总、免费包和付费包，支持分页、资源 ID 去重、每日切片及订阅档优先级。国内纯免费账号跳过付费包查询。小数使用精确合计，采集快照的 `RemainingPrecise` 保留六位小数；号池仍按既有积分单位四舍五入成整数。企业余额继续使用已兼容的旧接口。
- 模型优先读取 `/v3/config`，有企业 ID 的账号再补充本企业的 `config/models`。公共目录同名条目优先，禁用及非对话条目不纳入；返回列表按当前账号生成，不跨账号合并。
- 新读取接口不支持或响应结构不兼容时可回退旧接口；认证失败、限流、代理不可用及调用取消不触发旧接口重试。资源包分页失败不写入部分余额，采集频率及账号代理路由不变。
- 支持 gzip、zlib/裸 deflate、Brotli 和 Zstandard 响应解码，控制面大小限制作用于解压后的正文；SSE 保持惰性读取及既有超时、取消机制。
- 刷新诊断保留错误分类而不打印上游正文；等待刷新期间安装了更新凭证时，旧结果不能覆盖或误报成功。设备档案、账号换号策略和部署状态不受本次兼容改动影响。

协议差异核对参考 [yreidev 的客户端研究说明](https://github.com/yreidev/workbuddy2api-panel/blob/39049fcc479c1240d0d80d0404e0308f651424de/docs/official-client.md)。该说明的研究或实测范围不等于本项目已完成真实上游验证。

### 无缝业务升级（Linux 单主机）

默认使用下述串行交接。新增显式 `overlap` 模式：新推理请求立即切到候选版本，旧版本继续完成已有请求；管理和后台保持单一 owner。该模式要求 PostgreSQL 与同机共享持久目录，首次迁移仍需串行交接一次。参数和限制见下文「overlap 并行交接」。

新增 `handoff-gateway` 稳定入口和业务生命周期协议。升级时入口暂存新请求，等待旧聊天/SSE 与已接收的后台任务结束，提交状态后退出旧版本，再启动新版本释放队列。排空超预算会恢复旧版本接单；不强杀已有流、不重放可能已经生成的请求。

这是**短时排队的单写交接**。请求必须在客户端、NewAPI、各层反代与 gateway 的共同等待预算内完成；队满或等待到期返回 503。无限长流、数据库重大迁移、宿主机/gateway 自身升级和崩溃恢复不承诺无中断。首次接入入口及首次从不支持本协议的旧版本迁移，需单独安排。

构建与测试按当前工作区要求在 SSH `tyy2` 宿主机执行，先检查连通性与数据盘，加载 `/srv/build-server/env/workbuddy2api.sh`，使用隔离源码目录。发布时使用下文「宿主机发布构建与许可打包」的完整流程；运行镜像只打包宿主机产物：应用用 `Dockerfile` / `Dockerfile.local`，稳定入口用 `Dockerfile.gateway`。

宿主机构建命令（在 tyy2 项目环境内）：

```bash
go test -p=2 ./...
go build -o "$ARTIFACT_DIR/wb2api" ./cmd/server
go build -o "$ARTIFACT_DIR/handoff-gateway" ./cmd/handoff-gateway
python3 scripts/test-seamless-upgrade.py "$ARTIFACT_DIR/wb2api" "$ARTIFACT_DIR/handoff-gateway"
python3 scripts/test-upgrade-controller.py
```

部署前准备：

- 所有业务流量改为经过 gateway，业务端口仅回环/私有网络可达。参考拓扑：原 TLS 入口 → `127.0.0.1:7863` gateway → A `127.0.0.1:7864` / B `127.0.0.1:7865`。Linux 容器可使用 host 网络并显式绑定回环；保留原部署全部必要挂载。
- 在宿主机创建服务专用本地运行目录，例如 `/run/wb2api-upgrade`，权限 0700、属主与进程 UID 一致；所有版本映射同一目录。不可用 NFS。控制 socket 权限 0600，禁止映射为公网管理接口。
- 保留相同 config、auths、data、数据库、管理员凭证和显式 session salt。未配置 salt 时，升级模式从稳定管理员凭证按独立用途派生，不新增明文秘密文件。
- A、B 使用不同 release ID、业务端口、控制 socket，共用同一个 `-state-lock`。业务启动前即获取文件锁，第二个写入者会拒绝启动。部署工具只能在确认旧进程退出后启动 B。
- 对已准备的 A/B 容器使用 `restart=no`，避免宿主机重启时旧版本自行抢到写锁；发布完成后，由外部服务管理器只启动发布日志确认的活动版本。不要让两个版本同时自动重启。

业务启动参数示例（配置通过既有挂载/secret 提供，参数不含秘密）：

```bash
# A 与 B 的独立启动参数；WB2A_LISTEN 分别设为 127.0.0.1:7864 / :7865
wb2api -config /app/config.json -release release-a \
  -control /run/wb2api-upgrade/a.sock -state-lock /run/wb2api-upgrade/writer.lock
wb2api -config /app/config.json -release release-b \
  -control /run/wb2api-upgrade/b.sock -state-lock /run/wb2api-upgrade/writer.lock

# 先单独完成初次入口接入，后续业务升级不重启此进程
handoff-gateway -listen 127.0.0.1:7863 -target http://127.0.0.1:7864 \
  -release release-a -control /run/wb2api-upgrade/gateway.sock \
  -state /opt/wb2api-upgrade/gateway.state \
  -wait 90s -pending 128 -queued-bytes 64000000 -body-bytes 8000000
```

gateway 默认不信任转发头。只有本机受控 TLS 入口能访问 gateway 时，才加 `-trust-ingress` 保留其真实 IP/HTTPS 信息；业务端仍需显式信任 gateway 地址。gateway 将活动版本、目标地址和 hold 状态写入 `-state` 文件，重启读取已提交状态；若发布中退出，会保持 hold 供恢复检查，不自行猜测版本。省略 `-state` 时使用控制 socket 旁的 `.state`；生产应准备同 UID、0700 的持久目录（如 `/opt/wb2api-upgrade`），避免 `/run` 随宿主机重启被清空。锁、状态文件和发布日志不能在交接期间清除。队列不预读/落盘请求体；未知长度按最大请求体预留预算，额外计入每项 32 KiB 头部预算。`-body-bytes` 与应用请求体限制同步；大请求/慢客户端仍受各层超时与资源限制约束。

业务新增只读 `GET /livez`（存活）、`GET /readyz`（接单就绪），响应带 release ID；不触发上游请求。原 `/healthz` 继续表示账号健康。业务响应新增 `X-WorkBuddy-Release`。

只读候选检查：以 B 镜像另外创建一个**停止状态的预检容器**，与 B 相同环境、配置和挂载位置，挂载可设只读，入口参数改成 `-config /app/config.json -preflight`。预检只验证配置，不初始化生产存储、迁移、代理或后台任务；通过不代表真实业务已经就绪。

准备好 A（运行）、B（停止）、预检容器（停止）后，在 Linux 宿主机执行：

```bash
python3 scripts/seamless-upgrade.py \
  --old-container wb-a --new-container wb-b --check-container wb-b-preflight \
  --old-release release-a --new-release release-b \
  --old-control /run/wb2api-upgrade/a.sock --new-control /run/wb2api-upgrade/b.sock \
  --gateway-control /run/wb2api-upgrade/gateway.sock \
  --new-target http://127.0.0.1:7865 \
  --drain-seconds 20 --start-seconds 20 \
  --journal /opt/wb2api-upgrade/release-a-to-b.json
```

脚本先检查镜像、挂载、角色和 release，再预检、hold、排空、最终提交、等待真实退出、启动候选并核对实际目标的 `/readyz`，最后 switch/resume。发布日志原子写阶段，不包含凭证。`gateway wait` 至少覆盖排空 + 启动 + 15 秒提交/退出余量；生产客户端链路也必须允许该等待。

会话账号绑定、控制台 Cookie 摘要和解锁限流状态通过 `data/upgrade-handoff.enc` 做 AES-GCM 交接；保持原管理员凭证才能读取。Responses 历史继续使用原 SQL 存储。前缀预测缓存暂不交接，升级后重建；不保证上游 KV 命中率。后台任务采用**等全部已接收工作结束**的策略，不引入跨进程任务重放，长批任务可能使本次升级撤销。

失败与回滚：

- `aborted_old_serving`：排空预算不足或可撤销阶段出错，旧版本恢复接单。重新发布使用新 journal。
- `recovery_required`：可能已提交 stop、旧进程已退出，或 B 启动/切流失败。脚本不盲目重跑、不强杀、不恢复整库。先通过两个业务控制 socket、gateway `/status` 和容器 State 确认实际阶段；日志存在时脚本拒绝重跑。
- stop 回执丢失也视为状态未知。确认旧进程仍运行且未进入停止后，才能调用其 `/resume?release=...`，再恢复 gateway `/resume?expected=...`；已退出就不能只改入口指针。
- B 已开始业务后的兼容回滚使用同一协议，交换 A/B 角色，并准备 A 镜像的预检容器及新 journal。保留 B 新写入的数据。格式/迁移不向后兼容时另走维护流程。
- 控制命令通过私有 Unix HTTP socket 发出：gateway 支持 `/hold`、`/switch`、`/resume`（`expected` 参数）；业务支持 `/pause`、`/quiesce`、`/resume`、`/stop`（`release` 参数）。`quiesce` 要求前台已经排空，`stop` 要求前台后台均归零且最终持久化成功。均不是公网 API。

发布期间不删除旧镜像和备份，不顺手升级 PostgreSQL。旧页面可能仍引用前一版静态资源，发布制品应保留上一版带 hash 的资源；管理 API 不兼容变更需单独处理。

#### overlap 并行交接

两个业务版本都必须支持 overlap 协议，在原有 `-release`、`-control`、`-state-lock` 参数上增加：

```text
-upgrade-mode overlap -coordination-dir /opt/wb2api-upgrade/members
```

使用同一 Linux 宿主机、相同 UID、同一 PostgreSQL、相同的私有持久目录（0700）、凭证目录及状态目录。启动会检查配置摘要和目录设备号/inode，业务监听、前端资源和 pprof 可以不同。持久 `domain.json` 使 `/run` 锁目录重建后保持原 PG 协调域；不得删除它或绕过旧 writer.lock。共享协调不可用时拒绝操作，无 SQLite/内存降级。旧二进制需先按上面的串行流程迁移一次。

稳定入口增加 `-overlap`，发布脚本在上述命令上增加 `--mode overlap`。B 先以 standby 接受推理，再用 gateway 私有 `/switch-live` 原子切新请求到 B；已准入 A 的 HTTP/SSE 继续由 A 完成。账号名额、余额消耗、冷却、凭证刷新、会话、提示词状态和代理绑定由共享权威协调，代理的实际监听端口在各进程内独立。

重叠期间管理读取仍到 A，管理写入返回 503（解锁使用共享 Cookie/限流）；新推理没有发布 hold。原有全局容量与账号排队限制仍保留。A 前后台排空后，仅短暂 hold 管理入口，等待 A 最终提交并真实退出，B 才重读最终管理状态、接管后台。若候选启动后共享配置被旧后台改变，拒绝携带过期配置晋升。gateway 的管理等待预算至少为 `drain-seconds + 2 × start-seconds + 15` 秒。

切流后但 stop 提交前可以回切未来推理到 A，再等待 B 已准入请求完成。切流/stop 回执不明、晋升失败或 gateway 重启丢失计数时进入 `recovery_required`，不能自动清零旧活动或恢复整库；计划发布不强杀长流。此模式用于兼容版本的有限重叠，不支持跨机多活、入口重启不断流或不兼容 schema 迁移。

隔离验证使用假账号、本地上游桩和临时 PostgreSQL，在宿主机执行 `go test ./...`、共享协调的 `-race` 回归、`go vet ./...`、`scripts/test-upgrade-controller.py`、`scripts/test-seamless-upgrade.py` 与 `scripts/test-overlap-server.py`。后两个脚本需要宿主机构建的 server/gateway 路径；overlap 测试另从环境读取 `WB2API_TEST_POSTGRES_DSN`，只允许本机临时数据库。不要向这些测试提供生产凭证。

2026-11-10 已在 SSH `tyy2` 对当前未提交源码完成隔离验收：Go 全量回归 1657 个通过项（含子用例），共享逻辑 race 回归 46 项无跳过，`go vet`、两个宿主机程序构建、10 项发布控制器回归及串行/overlap 真实 HTTP/SSE 集成均通过。验证了 A 旧 SSE 未结束时 B 已处理新请求，A 完整排空退出后 B 接管后台，共享 Cookie 保留；也覆盖协调域在 `/run` 重建后不变。全量回归按原有开关跳过 12 项，其中流量与响应历史两个 PostgreSQL 用例已使用专用临时库补跑通过；其余为未配置 Redis、可选负载/性能与真实上游审计，以及 root 下无法验证的只读权限探针。未访问真实供应商、未部署。日志保留于 `tyy2:/srv/build-server/logs/workbuddy2api/overlap-20261110-12293583/`，宿主机产物位于对应 `artifacts/workbuddy2api/overlap-20261110-12293583/`。

控制台新增「采集中心」，统一执行积分余额、请求用量、凭证有效期、模型权限和成长任务状态的在线采集。账号管理中可以按单账号、已选账号、当前筛选结果或全池发起，任务离开页面后继续执行。刷新列表只读取本地状态；`/v1/models` 从按账号隔离的模型快照汇总，首次成功采集前回退静态模型表。

默认并发 4、每秒最多 4 次上游请求；在采集中心可调整并立即生效。默认自动周期：积分 15 分钟、用量/凭证 5 分钟、模型 1 小时、任务 30 分钟；可关闭启动及定时采集。自动采集跳过禁用账号，管理员可显式选择「包含禁用账号（仅查询，不启用）」。凭证临近到期或查询遇到 401 时才续期，禁用账号不自动续期。在线聊天鉴权恢复保留独立路径。

任务、逐项结果、设置和成功快照保存在现有 SQLite/PostgreSQL 状态数据库，保留最近 100 个历史任务。重启将未完成项标为中断，管理员可重试失败/中断项；取消只停止未开始项。失败保留上次成功值，未成功采集的积分显示未知。账号页区分上游积分与本地消耗估算。用量按账号水位增量抓取，回看 2 小时处理延迟计费，首次回看 48 小时；完整翻页完成后才推进水位。单次上限 500 页/2 分钟，超限报错并保留水位。

管理员采集接口（均使用管理员密钥或控制台会话，分组密钥不可访问）：

| 方法与路径 | 用途 |
| --- | --- |
| `GET /admin/collection` | 设置及最近任务摘要；只读 |
| `PUT /admin/collection/settings` | 保存 `concurrency`、`requests_per_second`、`automatic`、五种资源的 `intervals`（秒） |
| `POST /admin/collection/jobs` | 在线采集；JSON：`uids`（空为全池）、`resources`、`include_disabled`；返回 `job_id` |
| `GET /admin/collection/jobs/{id}` | 任务与逐项结果；支持 `page`、`page_size`（最多 100）、`status` |
| `POST /admin/collection/jobs/{id}/cancel` | 取消尚未开始项 |
| `POST /admin/collection/jobs/{id}/retry` | 新建仅失败/中断项的重试任务 |
| `GET /admin/collection/snapshots?uids=a,b` | 最多 100 个账号的新鲜度与错误；`data=1` 附加成功快照 |

`resources` 使用 `balance`、`usage`、`credential`、`models`、`tasks`。并发范围 1–16，速率范围 1–20 请求/秒，各周期范围 60–86400 秒。查询失败不会因余额采集而禁用账号。签到、领奖和发送真实对话继续使用各自操作入口，操作后只排入后续数据采集。数据库不可用时采集中心返回 503，不启动不受追踪的采集任务。

控制台提供通用的[系统提示词替换与 11128 诊断](internal/promptguard/README.md)：管理员可维护任意字面替换规则、查看完整拦截提示词、手动或自动二分定位触发句子，并验证替换后的完整系统提示词。自动诊断默认关闭。

### 1. 克隆 & 配置

```bash
git clone https://github.com/gao-gao-zai/wk.git workbuddy2api
cd workbuddy2api
cp config.example.json config.json
# 编辑 config.json，把 api_key 改成一个你自己生成的强随机值
# 例：openssl rand -hex 32
```

> `config.example.json` 里是**占位符**，服务会拒绝用它们启动。请务必替换成真实值。
> 程序也支持通过环境变量配置凭据；现有 compose 仍挂载 `config.json`，使用纯环境变量时应相应调整挂载与容器环境。

### 2. 添加账号

```bash
./login.sh
# 打开浏览器登录 → 按 y → 自动落盘 auths/ → 重启容器

# 添加海外版账号（中国区账号仍会保留）
./login.sh global
```

### 3. 启动服务

先按「宿主机发布构建与许可打包」在 tdog 生成完整发行包，将产物复制到部署机器的独立目录，再设置 `WB2A_RELEASE_DIR` 指向该目录。它必须包含 `wb2api`、`login`、`frontend/`、`source.tar.gz`、许可证与 `Dockerfile.local`，不能将源码仓库直接当作镜像构建上下文。

```bash
export WB2A_RELEASE_DIR=/absolute/path/to/release
docker compose up -d --build

# 使用 PostgreSQL（首次使用前请在 .env 里设置 POSTGRES_PASSWORD）
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d --build
```

### 4. 验证

```bash
# 模型列表
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer $WB2A_API_KEY"

# 账号状态（汇总 + 每账号详情）
curl -s http://localhost:7863/status -H "Authorization: Bearer $WB2A_API_KEY"

# 健康检查（公开路由，无需凭据；无健康账号时 503）
curl -s http://localhost:7863/healthz

# 聊天补全（流式）
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer $WB2A_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 聊天补全（非流式，本地聚合）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer $WB2A_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

除 `GET /healthz` 与 `POST /admin/unlock` 外，**所有**路由都需要凭据（`Authorization: Bearer <api_key>` 或控制台解锁 Cookie）；未携带凭据时返回 401。请求体必须是合法 JSON 对象，否则返回 `400 invalid_request`。

### 图片请求

支持 OpenAI 兼容的多模态 JSON 请求。图片可以使用公开 URL 或 Base64 data URL，建议使用上游已开放视觉能力的模型（例如 `glm-5v-turbo`）：

```bash
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer $WB2A_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5v-turbo","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"请描述图片"},{"type":"image_url","image_url":{"url":"data:image/png;base64,<BASE64>"}}]}]}'
```

当前接口接收 JSON，不提供 multipart 文件上传接口；请求体上限默认 8 MB，可通过 `max_request_body_mb`（1-128）或控制台「管理设置 → 请求体大小限制」修改，WebUI 保存即时生效。图片模型能力由 WorkBuddy 上游账号决定，格式不完整时服务会返回 `400 invalid_image`。

## Anthropic Messages 兼容接口

模型发现也兼容 Anthropic：`GET /v1/models` 和 `GET /v1/models/{model_id}` 接受 `x-api-key`；使用 Bearer 时增加 `anthropic-version` 请求头即可选择 Anthropic 格式。列表返回 `data`、`has_more`、`first_id`、`last_id`，支持 `limit`（默认 20，1–1000）及互斥的 `after_id` / `before_id`。元数据复用现有模型快照，未知能力返回 `capabilities: null`，不虚构上游能力。普通 Bearer 请求保持 OpenAI 格式；无 `/v1` 前缀的 `/models` 也支持相同功能。模型发现独立于 Messages 开关，沿用相同分组密钥作用域。

默认关闭。设置 `features.anthropic_api: true`、环境变量 `WB2A_ANTHROPIC_API=true`，或在控制台「管理设置 → 特性开关」启用；控制台保存后立即生效并写回配置。关闭时已鉴权请求返回 404。接口使用与 Chat 相同的账号池、鉴权和分组权限，接受 `x-api-key` 或 `Authorization: Bearer`。

```bash
curl http://localhost:7863/v1/messages \
  -H "x-api-key: $WB2A_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -d '{"model":"glm-5.2","max_tokens":1024,"messages":[{"role":"user","content":"你好"}],"stream":true}'
```

支持文本、URL/Base64 图片、客户端工具定义和调用、工具结果、常见工具选择及流式和非流式响应。流式工具参数会在完成后按工具分别输出，以保证并行调用不串块。`thinking` 预算和 `output_config.effort` 近似映射到 `reasoning_effort`，具体效果由模型决定；不生成 Anthropic 签名 thinking 块，历史 thinking 块不会转发。`cache_control` 不映射为上游缓存断点；`top_k` 不转发。Anthropic 服务端工具、结构化输出格式、文档和其他未支持的内容块会返回 400，而不会伪装成原生支持；不提供 `/v1/messages/count_tokens`。`strict: true` 工具默认允许静默降级为普通工具（不保证 schema 强约束）；设置 `features.anthropic_strict_downgrade=false`、`WB2A_ANTHROPIC_STRICT_DOWNGRADE=false` 或关闭控制台「Anthropic strict 静默降级」后，带该约束的请求返回 400。该开关保存后即时生效。流式转换每 10 秒发送 ping，避免推理或缓冲工具参数期间空闲超时；保留请求追踪及防缓冲响应头。计费和缓存统计依赖上游提供的 usage，缺失时不会凭空估算。停止序列触发情况及部分上游专有语义无法完整还原。

## 配置说明

> **权威字段定义见 [`config.example.json`](config.example.json)**：它是当前 schema 的唯一权威，下方样例与之保持一致。`cp config.example.json config.json` 即可得到完整默认配置。

生产部署必须设置非空 `api_key`（也可使用 `WB2A_API_KEY` 环境变量）。服务启动时会校验凭据，并拒绝以下三种情况，避免意外以无鉴权模式暴露接口：

- 配置文件不存在且未提供 `WB2A_API_KEY` / `WB2A_FRONTEND_PASSWORD`；
- `api_key` 与 `frontend_password` **同时为空**（此时没有任何东西可以校验，所有路由一律 401）；
- 使用了公开仓库里的占位符（`your-api-key-here`、`change-this-password`、`changeme` 等）。

`api_key` 与 `frontend_password` 只需设置其中一个：只设前者时控制台用 API Key 解锁，只设后者时控制台用密码解锁。

### 监听地址

默认只监听回环 `127.0.0.1:7863`，供本机编辑器/脚本使用。需要对外暴露时显式设置 `listen`：

```json
{ "listen": ":7863" }
```

容器镜像里已预置 `WB2A_LISTEN=:7863`（否则端口映射不生效）。

### 反向代理

服务默认**不采信** `X-Forwarded-For` / `X-Real-IP`，直接用 TCP 层的 `RemoteAddr` 作为客户端地址（用于控制台密码的错误次数限流）。这可以防止攻击者靠伪造这些头把每次尝试放进新的限流桶。

如果服务确实只在反向代理后面可达，可以显式声明代理地址（支持单地址或 CIDR）：

```json
{ "trusted_proxies": ["10.0.0.1", "172.17.0.0/16"] }
```

或 `WB2A_TRUSTED_PROXIES=10.0.0.1,172.17.0.0/16`。声明之后才会按 XFF 还原真实客户端 IP，并允许代理通过 `X-Forwarded-Proto: https` 让会话 Cookie 带上 `Secure`。

### 静态控制台

控制台只放行 `index.html` 与 `assets/index-<hash>.{js,css}`；目录下其它任何文件（包括误放的 `config.json`、备份、编辑器临时文件）一律 404，不再提供目录列表。资源目录可用 `frontend_dir` 或 `WB2A_FRONTEND_DIR` 指定（默认 `frontend`）。

```json
{
  "listen": "127.0.0.1:7863",
  "api_key": "your-api-key-here",
  "frontend_password": "change-this-password",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "region": "cn",
  "frontend_dir": "frontend",
  "trusted_proxies": [],
  "max_request_body_mb": 8,
  "cooldown": {
    "soft_rate": "60s"
  },
  "schedule": {
    "checkin_hours": [9, 21],
    "keepalive_hours": [22]
  },
  "upstream": {
	"header_profile": "desktop",
	"client_version_cn": "5.9.3",
	"client_version_global": "5.9.4",
	"cli_version": "2.165.0",
	"model_optimization": false,
	"refresh_identity_check": "best_effort",
    "timeout_seconds": 120,
    "stream_timeout_seconds": 0,
    "stream_idle_seconds": 120
  },
  "features": {
    "sanitize_blacklist_fingerprints": true,
    "passthrough": false,
    "codex_compat": false,
    "dsh_compat": false
  },
  "billing": {
    "input_credits_per_1k_tokens": 0,
    "output_credits_per_1k_tokens": 0,
    "cached_input_credits_per_1k_tokens": 0
  },
  "upstash": {
    "url": "",
    "token": ""
  },
  "postgres": {
    "dsn": "",
    "max_open_conns": 16,
    "max_idle_conns": 8,
    "conn_max_lifetime": "30m",
    "conn_max_idle_time": "5m",
    "fallback_to_sqlite": false
  },
  "pool": {
    "max_in_flight": 3,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m",
    "breaker_cooldown_max": "6h",
    "idle_weight_per_hour": 0.5,
    "idle_weight_max": 5.0
  },
  "session_sticky": {
    "enabled": true,
    "ttl": "30m",
    "gc_interval": "5m",
    "salt": "",
    "max_entries": 10000
  }
}
```

`session_sticky.salt` 是给会话键做 HMAC 的密钥（hex，至少 16 字节）。生产环境可显式设置，生成方式：`openssl rand -hex 32`。留空时从管理员 API Key（无 API Key 则用控制台密码）按独立用途派生稳定盐；均未配置时才生成进程内随机盐并告警。备份恢复、重启及平滑升级需保留原盐或管理员凭证，否则会话摘要改变，原绑定无法命中。

`session_sticky.max_entries` 是绑定表上限（默认 10000）。TTL 只能回收"已经过期"的条目，无法限制单个 TTL 窗口内涌入的大量新键，因此需要这个显式上限；到达上限时会淘汰最旧的一批绑定。

### 缓存感知路由

启用 `session_sticky` 时，`cache_routing.enabled` 默认开启。账号暂时满载不会删除会话绑定；请求先在有上限的队列中等待，有相近前缀缓存的备用账号更快时可临时借用。等待超时或队列已满后允许普通选号以维持可用性，临时借用不会覆盖主绑定。账号失效或当前模型冷却时才通过版本检查迁移绑定；旧并发请求的完成不会把新绑定改回去。

短期活跃保护与 30 分钟会话绑定独立：成功请求建立 5–15 秒的保护期，根据成功请求间隔调整；已有热会话的请求运行时保护继续有效，失败、取消或排队本身不续租。记录按鉴权身份、会话与规范化开场前缀隔离；更换开场内容不能继承旧前缀的优先权。保护记录最多与 `max_prefix_entries` 相同数量，不保存正文或密钥。

新会话优先选择预计占用压力较小、近期热会话较少的账号。在每账号并发上限为 3 时，默认软预留 1 个名额给近期会话或具有相关前缀缓存的请求。只有存在另一授权范围内、模型健康且有空闲名额的账号时才阻止无关新请求使用预留名额；全池拥堵、替代账号冷却或授权仅剩本账号时允许借用。检查与占名额在同一锁内，排队不能绕过检查。已开始的请求不抢占，预留由所有近期会话共享，不保证每个会话立即获得名额；并发上限为 0 或 1 时不预留。

队列默认按可信鉴权范围轮转，再在该范围内按会话轮转。缓存相关请求获得有限优先权，连续优先最多 2 次后让普通请求竞争；等待达到 `queue_aging` 的请求按最早入队优先，避免持续被新请求超越。身份为空的旧调用保留 FIFO；普通选号不能插队。这里是按请求轮转的公平策略，不能保证不同 token 长度的请求具有相同计算份额。网关若把多个下游用户汇聚到同一上游鉴权密钥，第一层只能识别该共同鉴权范围，不把客户端自报 `user_id` 当可信用户身份。

账号路由、提示词候选选择和会话公平队列共用 `internal/session` 的会话身份；请求在随机替换之前解析一次，后续功能从请求上下文读取。优先读取 `metadata.conversation_id`、`conversation_id`、`conversation`、`prompt_cache_key`，以及 `X-WorkBuddy-Session-ID` / `X-Session-ID` / `X-Conversation-ID` 请求头。标识需在同一对话中保持稳定，并由网关实际转发；不把 `metadata.user_id` 单独当作会话标识。身份按鉴权密钥身份与分组隔离，账号绑定再按规范化模型隔离，提示词选择可跨模型沿用。Responses 的 `previous_response_id` 通过历史记录恢复原会话标识。

缺少显式标识时，共享解析器根据随机替换前的规范化 system、tools 与第一条 user 消息生成会话摘要；追加后续消息不改变身份，修改开场或裁剪历史可能改变身份，相同开场仍可能合并。没有用户消息及显式标识时不建立会话绑定。账号的自动推断绑定仍遵守 `cache_routing.enabled` / `shadow` 开关。实际出站正文另外用于完整历史的前缀缓存匹配，不用于重算会话身份。候选索引只保存带秘密盐的摘要和使用量，最多保存 256 个自适应前缀检查点，不保存请求正文。前缀索引是进程内的近似记录，并不等同于上游真实 KV 状态。

账号与提示词绑定共用锁、过期、容量、版本检查和写入回滚实现，业务策略分别配置：账号可因失效/模型冷却迁移，提示词不随该迁移或权重刷新改变。账号保持原 Redis 异步镜像及加密交接，提示词保持原加密文件持久化。首次请求在权限和模型健康校验通过时兼容读取旧账号绑定；旧提示词绑定按需原子迁移，沿用候选而不重新抽样。新格式提示词绑定无法由旧版识别，版本回退需同时恢复升级前的加密状态备份。扩展约定见 [共享会话核心](internal/session/README.md)。

`config.example.json` 中的 `cache_routing` 配置：

- `wait_timeout`：整个请求在账号队列上的累计等待预算，默认 `5s`，请求取消/截止时间优先。
- `max_pending_per_account` / `max_pending_total`：每账号与全池等待人数上限，默认 `8` / `64`。不提高 `pool.max_in_flight`。
- `max_candidates`：缓存候选数，默认 `3`。
- `max_cache_loss`：优先候选相对最佳预测缓存的容许损失比例，默认 `0.1`。这不是实际命中率保证；等待预算耗尽后可以放宽以继续服务。
- `prefix_ttl` / `max_prefix_entries`：候选索引有效期和记录数上限，默认 `30m` / `2048`。
- `initial_prefill_tokens_per_second`：缺少实测样本时使用的初始估计，默认 `5000`，不是生产测量值。成功流式请求的 usage 和首包时间会更新估计。
- `shadow`：默认 `false`。设为 `true` 时只记录前缀选择建议，不启用前缀选号、自动会话提示或排队；绑定版本保护与模型/权限隔离仍生效。
- `session_protection`：默认 `true`，控制活跃租约、新会话压力选号和软预留；设为 `false` 不改变既有主绑定。
- `active_lease_min` / `active_lease_max`：默认 `5s` / `15s`，保护期范围；上限必须不小于下限且不超过 `1m`。
- `reserved_slots`：默认 `1`，实际预留不超过单账号并发上限减 1；通过 `session_protection=false` 关闭。
- `fair_queue` / `queue_aging`：默认 `true` / `2s`，控制身份/会话轮转及久等请求优先；关闭 `fair_queue` 后保留 FIFO。
- `max_pending_per_session`：公平队列下，同一鉴权范围的单会话在单账号上最多等待的人数，默认 `4`，不超过该账号总等待上限；限制单会话等待堆积。

这些参数通过配置文件在启动时加载。`enabled=false` 关闭前缀索引、等待与活跃保护；`session_sticky.enabled=false` 则关闭整个会话路由功能。生产启用前可先用 `shadow=true` 观察（影子模式也不启用活跃保护和公平队列）。`[cache_route]` 日志包含会话键来源、键/账号摘要、绑定版本、路由原因、预测/实际缓存、实际缓存是否存在、排队时间以及实际账号当前 `active_sessions` / `hot_sessions` 数量；不输出会话 ID 原文或密钥。

仪表盘缓存率与 token 卡片使用同一时间窗口，按 `sum(cache_read_tokens) / sum(input_tokens)` 计算；空窗口显示 0%。上游 SSE 分多帧提供 usage 时，缺失字段保留之前的值，明确提供的零值会覆盖。

`region` 支持 `cn`、`global` 和 `all`。`cn`/`global` 只加载对应区域的授权文件，`all` 会同时加载中国区与海外版账号。控制台“账号授权”中选择“海外版”完成登录后，如果当前配置是单区域，服务会自动将配置切换为 `all`，保证新账号在重启后仍然可用；使用脚本时可执行 `./login.sh global`。

也可以直接调用登录辅助程序：`./login url global` 获取海外版授权链接，浏览器完成授权后执行 `./login poll global`。中国区仍可使用原来的 `./login url` / `./login poll` 用法。两种区域使用独立的临时 state 文件，交替登录不会互相覆盖。

正常浏览器授权采用产品登录流程：中国区 `platform=workbuddy`，海外版 `platform=workbuddy-ai`；中国区用 `/v2/plugin/account` 读取账号。升级前已经发起的 CLI 授权会话仍可继续完成。辅助程序按官方后台服务的形状发请求——UA 为 `<应用名>/<版本> <应用名>/<版本> CLI/<cli>`（国内 `WorkBuddy/…`、国际 `workbuddy-ai/…`），带请求框架公共头与 `X-Custom-Channel-Id: unknown`，不发 `Origin`/`Referer`；每次登录生成 `loginSessionId` 与链路头，国内版发起授权另在授权链接上追加 `version`、`traceparent`、`loginSessionId`。`X-Device-Token` 来自腾讯图灵盾原生 SDK（只有 macOS/Windows 实现），网关做不出来，不伪造。短信直登和自动加号继续使用原有链路，仍是旧形状。面板仅在明确返回“等待授权”时继续轮询，授权被拒或接口失败会显示错误；错误只包含阶段、HTTP 状态和业务码，不回显凭据或上游响应体。

**注意**：`cooldown.hard_credit` / `cooldown.err_threshold` / `cooldown.err_cooldown` 三个历史键已退役。硬冷却固定为**次日 04:00**（本地时区，`CooldownUntilTomorrow4AM`），连续错误语义并入熔断器（`pool.breaker_threshold` 触发指数退避）。旧配置中的这些键因 JSON 未知字段被自然忽略，不报错。

### 上报设备与标识

桌面事件的 Windows 版本、CPU 核数和内存由账号 UID 稳定伪随机分配，允许不同账号选到相同硬件组合。同一 UID 在重启、凭证刷新、账号导入及代理迁移后保持相同档案，无需新增状态文件；小程序上报沿用该档案对应的 Windows 主版本。设备档案 v1 的候选表及选择规则固定，后续修改必须考虑已有账号的一致性。事件业务字段不能覆盖公共设备字段。

上报设备标识在首次需要时生成随机的非敏感本地身份，原子保存到原授权文件的 `_device_identity` 元数据，再按不同上报用途派生标识，不再直接由 UID 推导。初始化保留授权文件中未知的供应商字段，权限仍为 0600；刷新和内置加密导入/导出保留该身份。旧账号首次使用新版本时会迁移一次设备标识，原硬件组合不变。授权文件只读、无法保存或账号仅在内存时沿用旧标识规则，避免临时随机身份随重启变化或阻塞聊天。备份与迁移应保留原授权文件及该元数据。

设备平台按账号保存，支持已有 Windows 与新 Mac 账号混用，不设把全池切换平台的全局开关。通过 OAuth、短信登录或自动加号**首次创建**账号时，默认生成 Mac 档案：`darwin/arm64`、Darwin `24.6.0`、Apple M2 8 核，内存在 8/16/24 GiB 中随机选一次。M2 配置范围依据 [Apple 技术规格](https://support.apple.com/en-ie/111867)；Mac 请求字段参考 [客户端研究记录](https://github.com/yreidev/workbuddy2api-panel/blob/39049fcc479c1240d0d80d0404e0308f651424de/docs/official-client.md)。具体硬件、机器标识和浏览器标识保存在授权文件的 `_device_profile`，随机种子继续使用 `_device_identity`。不同账号可以有相同硬件，每个账号的已分配参数不因重启、版本默认值、刷新 token、重新登录或代理变化而重选。当前不向新增账号开放 Windows 选择，后续研究完成后再新增创建选项，不迁移已有设备。

已有授权文件没有 `_device_profile` 时，直接从 `auth_dir` 加载和重新登录仍使用原来的 Windows 兼容规则，保存时冻结原值，不批量迁移存量账号。通过面板/API **新导入**的账号（官方 JSON 或加密迁移包）缺少 `_device_profile` 时，按新账号规则生成并持久化 Mac 档案；已有 `_device_identity` 则保留并据此生成，缺失时才分配新标识。文件中已有合法 Mac/Windows 档案与扩展字段原样保留，已有 UID 或授权文件始终跳过。预览只展示设备处理计划，不生成或写入设备标识；无效设备元数据报错，不通过覆盖来修复。Mac 档案使用对应的桌面事件、SDK 系统/架构头和 Mac 浏览器 UA；Windows 档案保留既有参数，不套用 Mac 专属字段。SDK 头里的超时描述不改变网关实际超时设置。小程序通道目前只沿用旧 Windows 抓包协议，独立于桌面档案保留兼容行为，不声称已复现 Mac 微信。

`upstream.header_profile` 默认 `desktop`：聊天、刷新与已核对的积分/签到接口分别构造请求头。客户端版本集中在 `client_version_cn`（默认 `5.9.3`）、`client_version_global`（`5.9.4`）与 `cli_version`（`2.165.0`）；聊天及桌面事件使用对应区域版本，不沿用旧版本的 commit/releaseDate。账号原有设备档案继续使用。未核对的业务接口保持既有头部契约，独立登录工具和 TTFB 测速不在此配置覆盖范围内。可用 `WB2A_HEADER_PROFILE`、`WB2A_CLIENT_VERSION_CN`、`WB2A_CLIENT_VERSION_GLOBAL`、`WB2A_CLI_VERSION` 覆盖；`header_profile=legacy` 恢复旧头部和版本。版本值必须是安全的版本号，修改需重启。

`upstream.model_optimization` 默认 `false`，聊天发送 `X-Private-Data: true`，表示不允许用于模型优化。设为 `true` 只对个人账号发送 `false`；带 enterpriseId 或类型为 ultimate/exclusive 的账号仍发送 `true`。环境变量为 `WB2A_MODEL_OPTIMIZATION`，修改需重启。这是向上游声明的偏好，不是对上游数据处理的保证。

同一 Client 内重叠的凭证刷新会合并为一次请求，跨账号、区域及不同 token 不共享结果；等待者可独立取消，发起者取消会终止本次共享刷新。刷新响应先暂存，校验后再原子应用，并拒绝覆盖刷新期间导入的新凭证。`upstream.refresh_identity_check` 默认 `best_effort`（`WB2A_REFRESH_IDENTITY_CHECK`）：有 UID 的账号刷新后，以新 access token 查询国内 `/v2/plugin/account` 或国际 `/v2/plugin/accounts`，按 UID、enterpriseId 和已知账号类型选择身份；明确身份、已知授权来源或区域不匹配，以及身份接口拒绝新 token 时，不覆盖旧凭证。临时不可用或旧响应缺字段时，兼容模式继续刷新并仅记录脱敏原因；`strict` 要求校验成功，`off` 关闭额外身份查询。旧授权文件可不含新增元数据；已知 type、oneidAccountId、idp、departmentFullName、tokenType 会在刷新、保存和加密迁移时保留。该查询仍经过原账号代理路由，不改变换号重试条件。

`upstream.tls_fingerprint` 默认开启，通过统一 upstream.Client 发出的 HTTPS 使用 uTLS ClientHello，HTTP/1.1 连接继续复用。账号代理选择保持原逻辑；HTTP/HTTPS CONNECT、SOCKS5 隧道建立后再做同一套上游 TLS 握手，代理失败不新增直连回退。SNI、证书链和主机名校验使用实际目的地址，不使用样本中的域名；握手超时、调用方取消、流式空闲和总时长限制仍有效。HTTP 请求及注入的自定义 RoundTripper 保持原实现。启动时读取，改动后需要重启；设为 false 或 `WB2A_TLS_FINGERPRINT=false` 恢复标准库 TLS。需要自定义客户端证书或 VerifyConnection 回调的部署使用标准库 TLS。独立登录工具、TTFB 测速等自行创建 HTTP 客户端的路径不在此开关覆盖范围内。

TLS 握手样本数据来自 [yreidev/workbuddy2api-panel 39049fc](https://github.com/yreidev/workbuddy2api-panel/blob/39049fcc479c1240d0d80d0404e0308f651424de/internal/upstream/tlsmimic.go) 公开的 Electron/Node ClientHello 抓包，保存在 `internal/upstream/testdata/workbuddy_clienthello.hex`；每条新连接重新解析扩展对象，随机数、密钥交换材料和目标 SNI 由当前连接生成。只调整握手形状，不会把账号硬件、UA 或所有出站路径自动变成某个官方客户端，也不保证上游接受。连接和路由实现使用项目已有版本的 uTLS，不复制该分支的实现代码。

`upstream.shared_conversation_trace` 默认开启，普通 Chat Completions 及经其收口的 Responses/Messages 请求，在选号前从共享会话解析器的鉴权范围隔离身份生成出站会话 ID。该 ID 与选中的账号无关，同会话换号及追加对话保持一致，保留原会话盐可跨重启重建；没有可识别会话的请求单独生成 ID。每个下游 HTTP 请求建立新的轮级追踪 ID，本次换号重试共用；每次实际发送的消息 ID、span ID 独立，B3 与 W3C traceparent 描述同一次尝试。不透传原始客户端会话或追踪 ID，不改变正文中既有会话字段剥离、账号粘性及换号条件。跨账号复用会话和追踪 ID 会明确关联这些尝试，并非账号匿名化；相同开场的推断会话仍遵循共享解析器既有合并规则。设为 false 或 `WB2A_SHARED_CONVERSATION_TRACE=false` 恢复旧请求头行为，改动后需要重启。

已执行真实桌面对话的任务读取到完成事件后才上报，使用实际输入长度、模型、usage、首 token 时间和完成时间；上游缺少的用量字段不填默认值。真实对话中的传输错误、SSE 错误或缺失完成事件不会因为提前收到 requestId 而被判成功。纯事件型任务保留兼容事件模板，不增加真实对话；任务文本、排程、测速设置及换号规则保持原行为。客户端版本字段集中维护，不随机改变协议身份。

程序生成的事件链、会话及请求标识使用随机 UUIDv4，不包含项目名前缀或本机时间戳；同一事件链仍保持关联，上游返回的真实 requestId 原样保留。客户端会话解析、聊天正文、账号换号、代理选择与任务排程规则保持原有行为。聊天 HTTP 错误的普通日志仅保留状态、错误分类、业务 code 和响应长度，避免记录上游回显的提示词或凭证；错误分类及客户端响应仍使用原始正文。

这些措施减少额外信息暴露，不保证消除出口、请求内容及行为时序之间的关联。

## 请求代理（reqproxy，实际请求账号的代理池）

与 `sms.proxy`（注册用代理）完全独立的**新模块**：让号池账号的日常 API 请求
（对话、token 刷新、签到、成长任务）走代理出口，避免全部账号共用一个服务器 IP。

- **内核**：内嵌 [Xray-core](https://github.com/XTLS/Xray-core)（MPL-2.0），支持
  vmess / vless（含 reality）/ trojan / shadowsocks / socks / http / wireguard。
  hysteria2、tuic、ssr 等协议不支持，订阅里遇到会跳过并在 WebUI 标注。
- **订阅**：支持 v2rayN 订阅（UA 强制 `v2rayN/6.60`），自动解析
  `subscription-userinfo`（流量/到期，90% 用量告警）。Clash YAML 格式暂不支持，
  会明确报错而不是解析出 0 个节点。可配多个订阅 + 手动批量导入并存。
- **槽位模型**：账号 → 槽位 → 节点。每个槽位是内核里一个固定本地端口
  （默认 31080-31999）+ 一条静态路由规则。换节点只动 outbound，端口不变——
  账号侧零感知、连接不断、IP 切换意图明确。
- **绑定语义**：账号第一次发请求时自动分配槽位（粘性）；节点被筛掉/消失时
  该槽位自动换指向（保留端口）。支持手动 pin 到指定节点。
- **筛选规则**（WebUI 实时可改 + 草稿预览模拟）：名称白/黑名单关键词、
  延迟上限、`cn`/`global` 分地区规则、共享度（账号/节点目标值）。
- **健康检查**：每 15 分钟经节点真实测速（cn→copilot.tencent.com，
  global→workbuddy.ai），连续 3 次失败摘除 10 分钟。首轮未测速的节点不参与分配。
- **失败语义**：有节点但全不合格 → 请求失败（结构化报错，**绝不静默直连**）；
  节点池为空（无订阅且无手动节点）→ 降级直连 + 事件告警。
- **零回归**：模块停用（默认）或 `reqproxy` 不配置时，所有账号请求与旧版
  行为完全一致（直连）。

**WebUI**：控制台「账号运营 → 请求代理」（`#/reqproxy`），订阅 / 节点池 /
筛选规则 / 槽位与绑定 / 事件流五个视图，全部免改配置文件。

config.json 基础设施参数（全部可选，不配即用默认值）：

```json
"reqproxy": {
  "state_file": "",
  "health_interval": "15m",
  "latency_timeout": "5s",
  "unhealthy_cooldown": "10m",
  "port_min": 31080,
  "port_max": 31999
}
```

模块开关、订阅、节点、规则、绑定全部存 `data/reqproxy/state.json`（WebUI 管理，
改完即生效，5 秒内落盘）。管理 API 挂在 `/admin/reqproxy/*`（见
`internal/server/reqproxy_handlers.go`）。

## 并发与性能

### RPM 与多维并发统计

控制台新增「流量统计」（`#/traffic`），仪表盘也显示同一套统计。支持全站、单账号、单 API key、账号分组、代理分组和单代理六个维度，展示实时/平均 RPM、当前/平均/峰值并发，以及成功/失败、成功率、输入/输出 token、缓存读写与命中率、工具调用、平均首 token、平均耗时和积分消耗。统计页可搜索名称或 ID、排序并展开用量详情；每 10 秒更新。

只读管理接口：`GET /admin/traffic?window=24h`，仅管理员密钥或控制台会话可访问。`window` 支持 `1m`、`5m`、`1h`、`6h`、`24h`、`7d`、`30d`。无需新增配置或手动迁移。

- **全站/key** 记录已通过鉴权的客户端模型请求，每个请求只计一次，包含请求体校验失败；鉴权失败、模型列表及管理操作不计入。
- **账号/账号分组/代理/代理分组** 记录模型请求的每次上游尝试，包含换号重试。凭证刷新、签到、成长任务、测速、健康探测等维护流量不计入。代理未选出时不归因给某个代理；直连单列。
- **实时 RPM** 是当前进程最近 60 秒的请求/尝试起点数。服务重启后实时窗口重新积累，已持久化的历史平均值保留。
- **平均 RPM** 为窗口内起点数除以窗口分钟数；**平均并发** 为窗口内在途占用总秒数除以窗口秒数，包含空闲时间。请求起止事件精确积分，跨分钟长流也持续计入；轮询频率不影响平均值和峰值。
- 历史汇总按分钟保存，所选窗口的起点向下对齐到分钟，页面显示实际起止时间。采集不足一个完整窗口时标记覆盖不足，并用采集开始至当前的实际时长计算平均值，不能将缺失历史当作已采集的零流量。
- 请求数按**发起时间**归窗，结果、token 和积分按**结束时间**归窗，因此跨窗口请求可能令请求数与成功/失败之和不同。平均耗时以已结束数量为分母，首 token 均值以有观测的样本数为分母，缓存命中率仅按已返回缓存信息的样本计算累计读取 token / 累计输入 token，缺失样本显示空值，展开详情可查看样本数。
- 全站/key 耗时包含请求校验、排队、刷新和所有重试；账号/代理耗时从本次模型尝试开始计时。流式请求持续占用并发直到响应结束。
- 分组和代理归属按尝试发生时捕获，之后更换归属不回写历史。账号多组归属会计入每个组，组统计不能直接相加作为全站总量。旧 key 或节点删除后仍可查看窗口内已记录的历史身份。
- 积分区分上游真实值、估算值和未知请求数；未知不作为确认的零成本。统计只保存身份 ID、脱敏名称和数字，不保存 key 正文、代理凭据、提示词或回答。

分钟统计使用当前选定的 SQLite/PostgreSQL 指标数据库，保存至独立的 `traffic_buckets` / `traffic_runs` 表，固定保留最近 **30 天**，不受请求日志条数限制或清理影响。每 10 秒批量写入，正常停机等待请求收尾后再次写入；异常终止可能丢失最近尚未写入的片段。统计表初始化失败时退化为内存统计，页面明确提示重启会丢失；运行期间数据库暂不可用时保留待写入数据并重试，查询返回不可用提示。

这套实时/并发统计面向单个服务进程。不同进程的持久桶用独立运行 ID 防止覆盖，历史计数可累加；多实例部署的实时并发和峰值不是跨实例同步聚合。新增维度从此版本启动后开始采集，旧请求日志无法补齐 key、代理、峰值并发等历史。

### 账号并发容量

`pool.max_in_flight` 默认是 **每个账号 3 个在途请求**。例如 5 个健康账号对应最多约 15 个同时转发的聊天请求；100 个健康账号对应约 300 个。流式请求会占用名额直到响应结束。模型冷却、禁用和上游限流会降低可用容量；`0` 表示不限制本地账号名额，不表示上游无配额。缓存路由开启时使用有界等待；等待或回退后仍无名额时返回 503。

`GET /status?model=deepseek-v4.1-flash` 的 `concurrency` 字段提供：

- `max_in_flight_per_account`：每账号配置上限。
- `configured_slots` / `available_slots`：指定模型健康账号的总名额 / 剩余名额；不含冷却兜底尝试。
- `in_flight`：全池所有模型当前在途请求数。
- `unlimited`：是否配置了无限制；此时两个 slots 字段为 0，应结合本字段解释。

正常情况下，成功吞吐量还受平均响应时间限制：`请求/秒 ≈ 可用并发名额 ÷ 平均响应秒数`，并且不能超过上游 RPM/TPM/账号配额。不要仅为增大数字就调高每账号上限。

本地性能优化包括：账号选择和名额占用合并为原子操作；SQLite 将统计、积分和请求日志合并到一个持久事务；SSE 使用按需增长的 4 KiB 初始缓冲；上游连接池保留更多空闲连接并采用标准 HTTP/2、拨号和 TLS 配置。请求 ID 仍原样透传。

指标和请求日志支持 PostgreSQL。配置 `postgres.dsn` 或设置 `WB2A_POSTGRES_DSN` 后，服务会使用带连接池的 PostgreSQL，并在同一事务中提交统计和请求日志；未配置 DSN 时继续使用本地 SQLite。PostgreSQL 连接失败默认进入内存指标模式，可将 `postgres.fallback_to_sqlite` 设为 `true` 以回退本地 SQLite。连接池可通过 `max_open_conns`、`max_idle_conns`、`conn_max_lifetime` 和 `conn_max_idle_time` 调整。

池状态与增长台账共用同一个 PostgreSQL DSN（`pool_accounts` / `growth_claims` 表）；PG 不可用时自动回退本地 SQLite（`data/state.db`），与指标的降级链相互独立。

首次切换到 PostgreSQL 时会自动创建表，已有 `metrics.db` 不会被删除或自动导入；切换前请保留该文件，需要历史数据时再安排离线迁移。池状态与台账同理：已有 `state.db` 不自动导入 PostgreSQL，需要时把 `data/` 目录下的旧库文件留好再安排迁移。

可运行不调用真实上游、不消耗账号额度的回环压测：

```bash
WK_LOADTEST=1 go test ./cmd/server -run '^TestProxyLoad$' -count=1 -v -timeout=120s

# 持续 30 秒、300 个客户端并发
WK_LOADTEST=1 WK_LOAD_CONCURRENCY=300 WK_LOAD_DURATION=30s \
  go test ./cmd/server -run '^TestProxyLoad$' -count=1 -v -timeout=120s

# 模拟 5 个账号、每个响应耗时 2 秒
WK_LOADTEST=1 WK_LOAD_ACCOUNTS=5 WK_LOAD_CONCURRENCY=15 WK_LOAD_DELAY=2s \
  go test ./cmd/server -run '^TestProxyLoad$' -count=1 -v -timeout=120s
```

默认使用 100 个模拟账号、每账号 3 个名额、1 KiB 文本请求、100 ms 上游 SSE 响应，并启用实际 SQLite 日志/积分写入。`WK_LOAD_POSTGRES_DSN` 可指定 PostgreSQL DSN 进行同样的持久化压测；`WK_LOAD_PER_ACCOUNT` 可改变模拟账号名额，`WK_LOAD_STREAM=false` 可测非流式聚合。输出的 `LOADTEST` JSON 包含成功 RPS、延迟分位数、HTTP 状态分布、实际最大上游并发和持久化计数。内存值是压测客户端、模拟上游和代理共同进程的 Go 堆峰值，不是生产服务器 RSS。它不包含公网 TLS、Redis、账号刷新、磁盘池快照、大图片或真实模型生成成本，也不是生产容量保证。

## 账号轮换与冷却策略

### 状态机

```
Healthy → Cooling → (签到恢复) → Healthy
   ↓           ↑
Disabled ←────┘ (session 死亡，永久)
```

### 错误分类

| 错误类型 | 冷却策略 | 恢复方式 |
|---|---|---|
| **402 + 余额关键词** | 冷却到**次日 04:00** | 签到任务（09:00/21:00）自动恢复 |
| **429 限流** | 有请求模型时只冷却该模型；普通 429 使用 60s 短冷却，上游 `code=6004`/携带 reset 时间时冷却到上游指定时间；缺少模型时才使用账号级冷却 | 到期自动恢复；明确限流模型在 reset 前不参与该模型的兜底 |
| **401 + session 死亡** | **永久禁用** | 人工重新登录 |
| **普通 401（包括 HTML 鉴权页）** | 账号独立隔离 60s，当前请求换号；后台最多一次 token 刷新，请求超时 15s | 刷新成功保存凭证，隔离到期后重试；临时故障保留隔离，不永久禁用 |
| **`invalid_grant: User disabled` / 明确账号异常受限的 403/11140** | **立即永久禁用** | 先解决上游账号限制，再人工恢复 |
| **404 上游偶发** | 60s 短冷却（不累计错误计数） | 到期自动恢复 |
| **5xx 上游故障** | 喂熔断计数（`pool.breaker_threshold` 触发指数退避熔断） | 熔断到期自动恢复 / 成功清零 |
| **网络抖动** | **不计失败**，立即换号重试 | 即时 |

### 挑选策略

1. **状态过滤**：Disabled / 账号级 Cooling / 当前模型 Cooling / 熔断 / 在途占满 不选
2. **Top-5 候选**：按三因子权重降序取前 5（credits 只是权重的一个因子，闲置补偿与成功率同样决定谁进短名单）
3. **三因子加权随机**：权重 = credits 比例 ×10 + 闲置补偿 + 成功率 ×3（credits 全 0 仍按闲置+成功率加权）
4. **防惊群**：跳过 100ms 内刚被选中的账号（除非 top5 全部刚被用过，退回 LRU）

## 账号池 v3

在 v2 基础上吸收外部项目成熟设计，引入四块能力：

- **熔断器（指数退避）**：连续 `pool.breaker_threshold` 次失败熔断，退避 `breaker_cooldown × 2^retryCount` 封顶 `breaker_cooldown_max`；成功清零。单一连续失败计数器 `fails`，签到解冻只清冷却（余额恢复）不动熔断——熔断作为"连续 5xx"信号要到退避到期或下次 chat 成功才恢复。
- **三因子加权选取**：`credits 比例 ×10 + idleWeight + successRate ×3`。闲置补偿每小时 `+idle_weight_per_hour`（封顶 `idle_weight_max`），成功率无记录给中性 1.5。
- **在途租约**：单账号并发上限 `pool.max_in_flight`（0 = 不限），`Pick` 跳过占满账号。
- **会话粘性路由**：使用共享会话身份尽量绑定同一账号，TTL 滚动续期；账号失效或模型冷却时通过版本检查迁移，满载时的临时借用不覆盖健康主绑定。提示词候选选择共用会话身份与绑定核心。
- **全冷却兜底**：无 healthy 账号时从普通软冷却账号选最早到期者顶班；禁用、余额耗尽和上游明确限流账号在恢复前永不参与。

401 鉴权隔离独立于余额、模型限流和熔断，`/status` 通过 `auth_until`、`cool_kind=auth` 显示；该状态会持久化，余额刷新、签到和旧的在途请求成功都不会解除它，全冷却兜底也不会选择它。同一账号在隔离窗口内只启动一次后台刷新；普通内容审核 11140 仍保留连续 3 次禁用的阈值。

上游返回 `code=6004` 或带明确 reset 时间的限流时，状态保存在账号的
`model_cooldowns` 中。该字段按模型记录恢复时间；账号仍可为其他模型提供服务，
`/status` 的账号级 `cooling` 不会因为单个模型限流而变为 `true`。

### Redis（Upstash）镜像

- 配置 `upstash.url/token`（空 = 纯内存模式，一切功能照常，只打一条启动警告）。
- Redis 仅做异步镜像（粘性会话映射防重启丢失 + 池状态快照恢复备份），**不在请求热路径同步调用**。
- 池状态快照：每次落盘同步镜像一份到 Redis（带 `saved_at`）；启动时**择新恢复**——Redis 快照比本地存储（`state.db` 或 `state.json`）新才采用，否则本地优先。
- `/status` 透出 `redis_mode`（`upstash`/`noop`）与池级 `sticky_sessions`。
- 账号状态同时包含上游 `capacity_size/remain/used`、`cycle_capacity_size/remain/used` 和 `credit_updated_at`；服务启动、定时签到或调用积分刷新接口时更新，并随池状态存储持久化。

### 池状态与增长台账存储

账号池状态与增长任务台账默认持久化在 `state_file` 同目录的 **`state.db`**（SQLite）；配置 PostgreSQL DSN 后（`postgres.dsn`，与请求日志共用）则写入 PostgreSQL。

- **池状态**（`pool_accounts` 表，每账号一行）：credits、容量、冷却/熔断、模型级限流、成功/失败计数等。落盘为**增量**——只写 5 秒周期内变更过的账号行，而非整文件重写（200 账号下每轮只写几行）。
- **增长台账**（`growth_claims` 表，`(uid, task_code)` 主键）：一次性任务领取记录与积分/能量收益，逐条 INSERT（幂等，重复领取保留首条）。
- **自动迁移**：首次启动发现旧 `state.json` / `growth-ledger.json` 且库为空时自动导入（启动日志可见 `已从 ... 迁移 N 个账号`）；**旧文件原样保留**，回滚到旧版本时直接可用（可能略旧，冷却态过期自愈）。
- **降级链**：PG 不可用 → 回退 SQLite；SQLite 打不开（卷故障/只读）→ 退化为旧 JSON 文件模式（零回归），启动日志明示。
- Redis 择新恢复的比较基准在 DB 模式下换用 DB 的最后写入时间（本地 `state.json` 不再更新，mtime 恒旧）。

### 请求日志、token 与积分

每个 Chat Completions 和 Responses 请求结束后都会写入配置的指标存储：默认是 `state_file` 同目录的
`metrics.db`，配置 PostgreSQL DSN 后则写入 PostgreSQL。记录只包含模型、路由、状态、账号 UID、token、耗时、积分来源和错误码，
不会保存提示词或模型输出，但会保存最多 1,024 字符的错误详情。保留策略为**天数+条数双条件**（`request_log_retention_days` /
`request_log_retention_rows`，任一命中即删；0 = 对应条件不限，双零退化为默认 1 万条），控制台「管理设置」可改、即时生效。
`GET /requests?limit=50` 可查询最近记录，控制台也会展示同一份数据。
控制台“请求日志”菜单支持按模型、端点、账号、错误、流式/同步/透传和成功状态筛选，并可展开查看缓存 token、工具调用与完整错误详情。

仪表盘的缓存率、平均首 token 与 token 卡片共用窗口汇总，分别按 `sum(cache_read_tokens) / sum(input_tokens)`
和 `ttfb_millis_sum / ttfb_samples` 计算。手动刷新重新计算时间起止点；请求日志筛选只接受最新查询的响应。
汇总查询失败或后端不提供汇总时，窗口统计明确显示不可用，不退回累计值或当前页合计；
后端汇总失败返回 `request_summary_unavailable`（HTTP 500）。

`GET /requests?since=...&until=...&include=summary` 汇总窗口内所有匹配的**已保留日志**，不受 `limit` 限制。
`summary.coverage` 返回全库现存日志的时间边界、请求起止点和保留限制。窗口起点早于现存日志时，
控制台会提示覆盖范围；该提示不把新建服务没有历史记录误判为已经删除数据。删除的历史不会自动恢复，
7 天或 30 天窗口也不能绕过条数保留上限。

窗口汇总另含 `credits_upstream`、`credits_estimated`、`credit_upstream_requests`、`credit_estimated_requests`
和 `credit_unknown_requests`。控制台显示上游值与估算值的合计、来源拆分及未知覆盖率，未知请求不视为零成本。
`cache_write_tokens` 仍兼容上游未命中/创建字段，页面将其标为“未命中 / 创建输入”，不据此承诺后续缓存复用。

stdout 同时保留一行便于排查的表格日志：

```
| #001 | 18:31:31 | deepseek-v4 | stream | 200 | uid=0851ce35 | TTFB=801ms | tok=60 | 23.5tok/s | total=2.6s | credits=0.24(upstream) |
```

字段说明：
- `#001`：请求序号（进程级 atomic counter）
- `TTFB`：首 token 到达时间（stream 模式）
- `tok`：输出 token 数（从上游 usage.completion_tokens 精确读取，非估算）
- `uid`：账号 UID 前 8 位
- `credits`：积分消耗及来源；`upstream` 为上游真实值，`estimated` 为本地费率估算

上游没有返回积分字段时，可用 `billing` 中三个“每 1,000 token 积分”费率估算。
默认值均为 `0`，此时不猜测消耗并在请求记录中标记为 `unknown`。

### 原始流透传

设置 `features.passthrough=true` 后，流式 Chat Completions 会原样转发上游 SSE 字节，
保留上游扩展字段，不再执行帧规范化或补写 `[DONE]`。启用总开关后，可在单次请求中
发送 `X-WorkBuddy-Passthrough: false` 临时关闭。请求体中的未知字段本来就会保留；模型别名、
`tool_choice`、`reasoning_effort` 和 `stream` 仍会按上游兼容要求转换。

### Codex CLI 兼容

设置 `features.codex_compat=true`（或环境变量 `WB2A_CODEX_COMPAT=true`）后，出站请求中
系统提示词里的 Codex CLI 身份句会被最小改写：

> "Codex CLI is an **open source** project led by OpenAI." → "Codex CLI is an **open-source** project led by OpenAI."

一句话只改一处连字符，用于绕开上游内容审核对该句的逐字指纹匹配（与 `sanitize_blacklist_fingerprints`
对 Claude 指纹的处理是同一机制、互不干扰）。句子不存在时原样透传，不做任何其他改动；
非 system 消息（user/assistant）中出现的同句不会被改写。默认关闭。

### DSH 兼容

设置 `features.dsh_compat=true`，或环境变量 `WB2A_DSH_COMPAT=true`，也可在控制台「管理设置 → 特性开关 → DSH 兼容」启用；默认关闭，控制台保存后即时生效并写回配置。

出站系统提示词中的固定身份句、Web GUI 说明和路径引用说明会作同义替换：

```text
You are an AI agent powered by DeepSeek Harness.
→ You are an AI agent running on DeepSeek Harness.
```

身份句兼容原句带句号或省略句号的形式。另对以下完整的两句话作精确匹配，仅将开头的 `interacting` 改为 `communicating`，保留 GUI 指代规则及原意：

```text
You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to "this page", "this GUI", or "this app" without naming another target, they mean this GUI.
```

路径引用说明精确匹配以下完整的两句话，保留 `@` 引用、相对路径基准和绝对路径含义：

```text
Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host.
→ Paths explicitly referenced by the user are prefixed with @. Resolve relative paths from the workspace root; absolute paths refer to files or directories on the host.
```

三项替换共用 `dsh_compat` 开关，保留周围文本、标点和空白。只处理 `system` 及归一化后的 `developer` 文本（包括内容数组的文本段），不修改用户、助手、工具消息或图片。Chat Completions 与 Responses 共用此处理；与 Codex 兼容及指纹脱敏开关独立。

## 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | OAuth 登录，落盘 auth 文件（落盘交给 `./login save`：已有账号沿用设备档案，新账号建 Mac 档案） |
| `./credit.sh` | 积分日报（美化输出） |
| `./credit.sh -json` | 积分原始 JSON |
| `./signin.sh` | 批量签到（遍历 auths/ 下所有账号） |

## API 端点

### 账号管理

控制台「账号管理」（`#/pool`）支持中国区/海外版账号搜索、状态/区域/分组/凭证筛选、跨页多选，以及批量启用、禁用、清冷却、分组调整、签到、保活和删除。默认每页 25 条，可切换 50/100 条；筛选改变会清空选择。启用、禁用、清冷却及分组使用服务端批量接口，每批最多 100 个账号、集中持久化一次；分组在服务端锁内完成读取和追加/移除，保留其他归属。签到、保活及删除继续逐账号执行，最多 4 个账号并发。保留逐项结果、取消后续操作和失败项重试。删除需输入 `DELETE`；签到、保活和刷新上游积分会明确确认真实上游请求。

账号详情通过管理员只读接口 `GET /admin/account/{uid}/details` 按需加载，支持手动刷新，展示账号归属与授权元数据、凭据是否齐全及到期时间、设备平台/硬件/档案来源、脱敏设备标识、运行状态和积分快照。仅管理员密钥或控制台会话可访问，响应 `Cache-Control: no-store`，不返回 token、授权文件路径或未知原始字段，不扩大 `/status` 的字段范围。旧文件没有设备档案时展示明确标注的 Windows 兼容推导值；查看详情不生成设备、不写文件，也不调用上游。导入隔离标记是状态丢失时的兜底信息，不代表账号当前一定停用；当前状态以运行态为准。

分组调整支持替换、追加和移出，遵守区域约束。实例间迁移使用「加密导出」和「导入账号」：密码至少 12 字符，迁移包经 PBKDF2-SHA256（600000 次）与 AES-256-GCM 加密，只包含所选账号凭据与分组。先解密预览、选择后导入，不覆盖已有 UID 或授权文件；新账号均停用，核对后手动启用。导出不会停用或删除来源账号，需在目标验证后自行处理来源。导入仅保留目标已有且区域兼容的分组，不迁移管理员密钥、代理、临时冷却和计费统计。「导出清单」仅包含展示信息，不含凭据。

管理员批量接口：`POST /admin/accounts/batch`，JSON 为 `uids`（1–100 个）、`action`（`enable` / `disable` / `clear-cooldown` / `groups`）；分组另传 `groups`、`mode`（`replace` / `append` / `remove`）。返回逐项 `results`（`uid,status,detail`）及 `stop`；部分账号不存在或区域不兼容时保留其他成功项，持久化故障停止后续批次。状态保存失败会明确提示内存已变更、需刷新核对；分组写入失败保持原归属。离开页面或取消只停止尚未提交的批次，已提交批次继续完成；请求/回执异常不自动重放，也不回退逐账号接口。请先发布支持此接口的服务端，再使用新版页面。

「导入账号」也支持单个官方 `auth + account` JSON 文件，包括没有扩展名的文件；无需迁移密码。分组策略可选保留来源映射、替换为目标分组、追加目标分组；未自定义时普通 JSON 使用兼容的 `default`，迁移包保留目标已有的兼容来源组。自定义组必须存在且区域兼容，不满足时在预览标明问题，导入时重新校验，不静默回退。单账号文件可覆盖昵称（留空保留原值，最多 128 个 Unicode 字符，不含控制字符）。预览展示最终分组、昵称与设备计划，改参数须重新预览；已有 UID 或授权文件跳过，新账号默认停用。分组保存失败会报告账号已停用导入但分组未完成，停止后续账号，不计入完整成功。原始凭据和加密迁移包共用校验及导入流程；未开启下述成长任务选项时，不会主动登录或探测上游。

导入参数新增「自动检查并完成未完成成长任务」，默认关闭，对应 `run_growth_tasks` 布尔字段。预览不执行任务；明确开启后，仅本次确定持久化成功新增的中国区账号排队，重复、失败、提交待核对和海外版账号不执行。任务会产生真实上游请求及必要的短对话，可能消耗用量，账号仍保持停用。响应逐项返回 `growth_job_id`、`growth_status` 与说明，成长任务排队或执行失败不回滚成功导入。面板通过只读 `GET /admin/growth/jobs/{id}` 跟踪当前任务，关闭页面停止查询，服务端任务继续。

成长任务的全池、单账号、导入自动任务与手动接受/领奖/单项完成共用执行池，默认最多同时执行 16 个账号；成长任务页可在 1–128 间设置，保存后即时生效。调小不会中断已准入任务，后续准入等待活动数低于新值。配置键为 `growth_concurrency`，启动环境变量为 `WB2A_GROWTH_CONCURRENCY`；管理员 `GET/POST /admin/config` 可读取/局部保存该值，落盘失败不改变运行预算。实际并发取决于可用独立 IP 和端口容量。私有路由与执行池使用同一预算，不存在额外固定 4 的限制；排队只解析路由，执行准入后才租用端口，保留端口段四分之一（最多 16 个、至少 1 个）供业务绑定和测速，容量不足明确报错而不占光端口；相同 UID 或代理服务器连接 IP 互斥，不同节点名称或端口不构成不同 IP。DNS 多个答案按资源集合保守互斥，IPv4 映射 IPv6 统一规范；无法确定连接地址时独占，直连账号共用一个直连资源。不会为判定互斥探测公网出口 IP。任务使用独立私有代理路由租约，执行结束或取消后释放，不改账号业务代理绑定。一次性已领取任务按上游状态及本地台账跳过，循环任务仍以上游当前状态为准；锁定、过期、不具条件及窗口外任务跳过，已达标未领取时直接领奖，不重放动作。全账号流水线保留重启恢复标记；同步单项请求不写成全账号恢复意图，重启后由用户按原操作重试，避免扩大执行范围。

管理员迁移接口：`POST /admin/accounts/export`（`uids,password`），`POST /admin/accounts/import/preview` 和 `POST /admin/accounts/import` 接受加密模式 `bundle,password` 或原始模式 `credential`（官方 JSON 对象），两种模式互斥；实际导入另传所选 `uids`。最多 1000 个账号、请求体最多 16 MiB、解密内容最多 10 MiB、单个账号凭据最多 64 KiB；账号分组不设导入专有的 100 组限制。分组密钥禁止调用，迁移响应 `Cache-Control: no-store`。仅导入其他区域的新账号时才持久化 `region=all`，已为 `all` 或同区导入不重写配置。账号池持久化失败不计成功并停止后续账号；提交结果不确定时返回逐项 `pending` 提示，保留带停用标记的凭据，须恢复存储后核对账号池及文件，不能直接删除可能已经提交的唯一凭据。

导入可选参数：`group_mode=source|replace|append`（默认 `source`）；替换/追加时传 `groups`（1–1000 个目标组），来源模式不传目标组；`nickname` 仅允许整个文件只有一个账号时设置。相同选项传给预览和导入。原始文件还可通过 `credential_json` 传递完整 JSON 文本（与 `credential` 互斥）；面板采用文本形式，保留未知扩展字段的大整数精度。预览行的 `device_action` 为 `generate_mac` / `preserve` / `skip`，`device_platform` 为计划平台，不返回设备标识。补全设备后的凭据仍受 64 KiB 限制；导入选项不能启用账号或触发上游操作。

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | Bearer | OpenAI 兼容聊天补全（流式/非流式） |
| `POST /v1/responses` | Bearer | Responses API 适配（流式/非流式、工具调用、`previous_response_id`） |
| `GET /v1/models` | Bearer / x-api-key | 模型列表；按请求头选择 OpenAI 或 Anthropic 格式，Anthropic 支持游标分页 |
| `GET /v1/models/{model_id}` | Bearer / x-api-key | 查询单个模型，鉴权及协议选择与模型列表一致 |
| `GET /status` | Bearer | 账号状态汇总（total/healthy/cooling/disabled + 每账号详情；详情含模型级 `model_cooldowns`） |
| `GET /requests?limit=50` | Bearer | 最近请求日志（`limit` 默认 50、上限 500；不含提示词和响应正文） |
| `POST /admin/credits/refresh` | 前端会话/Bearer | 异步刷新所有账号的上游积分明细，不执行签到 |
| `POST /admin/account/{uid}/enable` | 前端会话/Bearer | 手动启用账号并清除禁用/冷却状态 |
| `POST /admin/account/{uid}/disable` | 前端会话/Bearer | 手动禁用账号，停止新请求使用 |
| `DELETE /admin/account/{uid}` | 前端会话/Bearer | 删除账号池记录及对应授权文件 |
| `POST /admin/account/url?region=cn|global` | 前端会话/Bearer | 生成对应区域 OAuth 授权链接 |
| `POST /admin/account/poll?region=cn|global` | 前端会话/Bearer | 轮询并保存对应区域授权结果 |
| `GET /healthz` | 无 | 健康检查（无健康账号时 503） |

控制台「请求测试」通过管理员专用入口 `GET /admin/playground/options`、
`POST /admin/playground/responses` 和 `POST /admin/playground/chat/completions` 工作，
支持管理员会话或全局管理员密钥，分组密钥不可调用。它不会要求管理域名开放 `/v1/*`，
因此可以与管理域名禁止客户端 API 的入口策略共存。

测试请求使用 `{ "request": { ...标准协议请求体... }, "group": "", "account_uid": "" }`。
空选择使用自动路由；选择分组后限定该组，指定账号后只使用该账号，两者同时设置时取交集。
账号仍受禁用、模型冷却、并发与代理限制，不会因为测试而自动启用，也不会在指定账号失败后换号。
选项刷新只读当前快照，不触发模型探测、余额采集或凭证刷新；点击发送才产生真实推理请求。

页面提供两种协议与流式/非流式选择、系统指令、输出上限、Temperature、推理档位、
浏览器等待超时及手动取消。响应正文与错误分开展示，包含 HTTP 状态、错误类型/代码、
请求 ID、耗时、首 token、输入/输出及缓存用量；HTML/纯文本错误页按文本显示，
流式失败、未完成和缺少完成标记的断流不会标成成功。请求预览与复制诊断不包含认证头，
最近 10 次参数及状态只保存在页面内存，载入历史不会自动重发。

ZCode 等使用 OpenAI Compatible 提供商的客户端，Base URL 应填写
`https://你的域名/v1`（直连本服务时为 `http://服务器地址:7863/v1`），模型名称单独填写。
客户端会在 Base URL 后追加 `/chat/completions`；如果填写域名根路径，中间网关可能
返回 HTML 页面，使客户端误报“模型未返回任何内容”。检查响应应为
`text/event-stream`（流式）或 `application/json`（非流式）。
本服务也提供同鉴权的 `/chat/completions`、`/responses`、`/models` 别名，方便直连客户端。
上游流读取超时会返回 `upstream_timeout` 错误帧和 `[DONE]`；Responses 使用
`response.failed`，不会把中断保存为已完成的会话。

上游超时分流为两套，语义不同：

- `timeout_seconds`（默认 120）：**非流式**请求的整个请求上限，含读完响应体，同时也
  约束刷新 token、对账、签到等控制面调用。
- `stream_idle_seconds`（默认 120，`-1` 关闭）：**流式**请求两次收到数据之间的最大
  间隔。上游持续产出时永不触发，因此长回答可以一直流下去；只有上游卡住不发数据时才
  尽快失败。
- `stream_timeout_seconds`（默认 `0`＝不限）：流式请求的总时长兜底，用于防止跑飞的流
  长期占住账号租约。默认不限，因为把它当成唯一约束就等于给回答长度设了死限。

对应环境变量 `WB2A_TIMEOUT_SECONDS`、`WB2A_STREAM_IDLE_SECONDS`、
`WB2A_STREAM_TIMEOUT_SECONDS`；启动日志会打印三者生效值。

聊天/Responses 请求体大小上限默认 8 MB，可用 `max_request_body_mb`
（1-128）调整（超限返回 `413 request_too_large`）。控制台「管理设置 →
请求体大小限制」可在线修改，保存即时生效并写回 config.json；对应环境变量
`WB2A_MAX_REQUEST_BODY_MB`。请求体会完整读进内存再解析，调大前先确认
服务器内存足够——上限封顶 128 MB 也是因此。MB 按十进制计算（1 MB = 1,000,000 字节）。旧配置键 `max_request_body_mib`、环境变量 `WB2A_MAX_REQUEST_BODY_MIB` 和请求字段 `mib` 保留为兼容别名，升级后其数值也按 MB 解释；新名称优先。

上游 WorkBuddy 返回的请求 ID 会原值传给客户端：兼容响应体中的 `id`、`request_id`、
`requestId`、`requestID`、`record_id`、`recordId`、`recordID` 以及常见请求 ID 响应头。Chat Completions、Responses
和请求日志使用同一个上游 ID，不添加前缀，也不重新生成；只有上游完全未返回 ID 时才使用本地兜底 ID。

重复调用 `/v1/responses` 时应复用稳定的 `prompt_cache_key` 或 `conversation`；使用上一轮返回的 `previous_response_id` 时，服务会恢复该轮上下文并沿用会话路由。`conversation` 本身只用于账号路由，不会自动恢复历史。会话正文同步保存到 PostgreSQL 或本地 SQLite，内存只缓存索引信息，不需要配置 Redis 即可在重启后接续。

### Responses 会话历史存储

控制台「管理设置 → Responses 会话历史存储」可以自定义**闲置过期时间**、最大历史条数、**全部历史的总容量**和**单条历史大小上限**，保存后即时生效并写回 `config.json`。也可直接配置（以下示例为闲置 7 天、10 万条、总容量 2,048 MB、单条 32 MB）：

```json
{
  "response_history": {
    "ttl_seconds": 604800,
    "max_entries": 100000,
    "max_storage_mb": 2048,
    "max_entry_mb": 32
  }
}
```

- 缺省为 `ttl_seconds=3600`、`max_entries=1024`、`max_storage_mb=64`、`max_entry_mb=32`；四个字段都必须是正整数，旧配置缺少单条上限时使用 32 MB，局部配置或管理接口补丁保留其余字段。秒数上限 3,153,600,000，条数与两个 MB 字段上限均为 1,000,000,000。
- 一条记录对应一轮响应及其新增输入，不是整段对话；`max_entries` 仍限制所有轮次的总条数。历史通过父响应 ID 串联，同一根响应的所有分支视为一段会话，按会话最后成功保存时间进行 **LRU 整段淘汰**，不单独删除祖先或某条分支。路由用的 `conversation`/`prompt_cache_key` 相同不代表两段独立历史会自动合并。
- 单条上限与总容量各自独立，允许单条上限大于总容量，但保存时必须同时满足两者。单条超限，或整个会话（含所有分支）加入本轮后已经超过总容量/总条数上限时，拒绝本轮写入并保留原有历史，不挤出无关会话。全局超限时，先清理过期会话，再淘汰最久未使用且没有活跃请求的会话。
- 调小单条上限会淘汰包含超大记录的整段会话，调小容量或条数时同样以会话为单位清理；调大不会恢复已删除数据。活跃请求暂时保护其会话，调小限额可能短暂保留超额历史，租约释放后再清理；新写入仍需满足总预算，不会借保护机制继续突破容量。
- 容量统计是每条记录**压缩前**序列化后的 JSON 字节数之和（含消息、父 ID、路由及时间元数据），`1 MB = 1,000,000 字节`。压缩和图片去重不改变限额。它不是进程 RSS、磁盘物理占用、整个数据库或其他业务数据的限制；映射、索引、压缩工作区、临时请求与存储引擎还有额外开销。删除记录后数据库可能保留可复用空页，数据库文件不一定立即缩小。
- 配置 PostgreSQL 时复用现有连接池，使用独立的 `response_history_*` 表；未配置时保存在 `state_file` 同目录的 `responses.db`（SQLite WAL、同步提交）。已配置 PostgreSQL 但不可用时，仅在 `postgres.fallback_to_sqlite=true` 时使用本地库；持久层无法初始化会阻止启动，避免静默退回易丢失的内存历史。备份本地库时使用 SQLite 备份工具或停服后备份，运行中不能只复制主文件而忽略 WAL。
- 多实例共享同一 PostgreSQL 数据库时，共享一份历史及事务保护的条数/容量预算；各实例内存仅保留受限的索引。策略以最近一次成功保存/启动应用的值为准，多实例应配置相同的策略。本地 SQLite 仅供同一主机使用。数据库策略更新失败时管理接口仍通过兼容字段 `updated.response_history_remote_pending=true` 提示重新保存；控制台显示当前实际存储后端。
- `ttl_seconds` 现在表示**闲置过期时间**，默认仍为 3,600 秒：成功提交新一轮时刷新整段会话的活跃时间及闲置期限。读取、失败请求、重启均不续期。调小时间按会话最后活跃时间清理；调大后在下一次成功保存时采用新期限，已过期数据不能复活。不可变正文中的原始时间戳保持不变，实际过期判断使用独立的会话元数据。
- 从加载历史到上游响应完成期间，整个会话持有跨实例共享的保护租约，每 10 秒续期，进程崩溃后最多 30 秒自动失效。没有其他可淘汰会话且总预算不足时，新写入会回滚，不删除活跃会话或保留半条链。请求结束释放保护后会再次执行清理。
- 会话已过期或淘汰时返回 `400 previous_response_not_found`，不会将残缺上下文发给上游；存储不可用时返回 `503 response_history_unavailable`。客户端需要移除 `previous_response_id`，在 `input` 中携带完整历史重新开始；没有本地历史则只能开启新对话。
- 新记录在非流式响应返回或流式完成事件发送前同步提交。模型生成成功但历史未保存时，保留生成结果，返回 `history_stored=false` 和 `history_error`；容量/条数超限的错误码为 `response_history_capacity_exceeded`，数据库写入故障为 `response_history_unavailable`。流式结果在 `response.completed`/`response.incomplete` 的 `response` 对象中包含相同字段。这些是本代理的扩展字段；该轮 ID 不能用于后续追加。写入故障仅记录不含正文的错误日志。
- 升级时旧 SQL 记录会在原字节不变的前提下补齐会话索引，缺失祖先或成环的旧链会清理；升级前仅存在内存的历史无法跨重启保留。已有 Redis 历史在首次读取时沿父 ID 导入完整链，保留原始剩余期限；已迁入并淘汰的记录不会反复导入。新记录不再写入 Redis。
- 每轮独立压缩：文本、工具参数和 JSON 使用 Zstd 快速压缩（压缩后更大或小于 1 KiB 则保留原文）。图片不缩放、不转码；规范 Base64 数据 URL 拆为原始二进制，按 SHA-256 跨记录去重，恢复时保留原 MIME、位置和完整 Base64 字节。非规范、转义或不支持的编码保留在原 JSON 中进行无损压缩，不修改表示形式。
- 连续 15 分钟无成功续接的正文和图片由后台每分钟尝试 Zstd 9 级压缩，每批最多 4 条正文及 4 张图片；仅在体积更小时替换。后台压缩不持有写事务，解压并校验后条件更新，不覆盖最近成功续接的记录。恢复时校验完整原始 JSON 的 SHA-256，校验失败的记录不会发给上游。该方案保持原始内容，但不承诺上游一定命中缓存。
- 增大历史总容量不会提高单次请求体限制或模型上下文窗口；超期记录不能用于接续，写入、策略更新及后台每分钟清理。正文现在会落盘，应像其他业务数据一样保护数据库及备份；压缩不等于加密。

## 安全说明

- **默认拒绝**：除 `GET /healthz` 与 `POST /admin/unlock`，所有路由都必须携带 `Authorization: Bearer <api_key>` 或控制台解锁 Cookie。凭据未配置时不是"放行"，而是**全部 401**。
- **凭据校验**：启动时拒绝空凭据与公开占位符，见上文「配置说明」。
- **静态资源白名单**：控制台只放行 `index.html` 与 `assets/index-<hash>.{js,css}`，不提供目录列表，其它文件一律 404。
- **安全响应头**：所有响应带 `X-Content-Type-Options: nosniff`、`X-Frame-Options: SAMEORIGIN`、`Referrer-Policy: no-referrer` 与 CSP（`script-src 'self'`）。
- **控制台不持久化 API Key**：Key 只保留在当前页面内存中，刷新后需重新输入；历史版本写入 `sessionStorage`/`localStorage` 的值会在加载时清除。
- **请求体上限**：聊天/Responses 请求体上限默认 8 MB（`max_request_body_mb` 可调，1-128，WebUI 即时生效），且必须是合法 JSON 对象。
- **上游响应上限**：单帧 SSE 1 MiB、聚合正文 8 MiB，超限返回 `response_too_large` 并正常补 `[DONE]`。
- **日志净化**：客户端可控字段进入日志前会按 rune 截断并剔除控制字符，防止 `\r` 与 ANSI 转义伪造日志行。
- **手机号脱敏**：自动加号日志中的号码以 `138****8000` 形式记录（号池账号昵称即手机号）。
- **上游跳转白名单**：短信直登的重定向跟随只允许跳到已配置端点的主机与端口，以及 `codebuddy.cn` 子域，防止会话 Cookie 被跨域 302 带走。

## 稳定性设计

- **防雪崩**：上游 4xx/5xx 轮转重试（不直接返回），404 短冷却 60s 不累计失败
- **错误分流**：网络层错误不计失败（避免抖动连坐）；HTTP 5xx 喂单一连续失败计数器，达 `breaker_threshold`（默认 3）触发指数退避熔断
- **请求日志**：SQLite 持久化模型、token、积分、TTFB、总耗时和错误码，stdout 保留精简表格
- **连接池**：`MaxIdleConnsPerHost=20` 减少 TLS 握手
- **凭证续期**：token 临近过期自动 refresh，失败禁用账号
- **状态持久化**：池状态增量写 `data/state.db`（只写变更过的账号行），5s 周期异步落库，进程退出前强制 flush；库不可用时退化为 `state.json` 全量重写（零回归）
- **防惊群**：100ms 窗口内不重复选中同一账号（高并发时打散热点）

## 开发

### 测试

以下命令在 SSH `tdog` 的隔离源码目录、加载项目环境后执行；发行包使用 `scripts/build-release.sh`，不以单独编译代替源码及许可打包。

```bash
go build ./...
go test ./... -count=20  # 20 次全绿（无 flake）
go vet ./...
gofmt -l .  # 应为空
```

### 代码结构

```
cmd/
  server/     # 主服务入口
  login/      # OAuth 登录工具
  credit/     # 积分查询工具
  signin/     # 批量签到工具
internal/
  auth/       # auth 文件解析 + token 刷新
  pool/       # 账号池（状态机 + 冷却 + 持久化）
  scheduler/  # 定时签到 + 积分查询
  server/     # HTTP handler + 请求日志
  upstream/   # 上游 API 封装（chat/billing/auth）
```

## 免责声明

本项目按下述开源许可提供。使用者需遵守 WorkBuddy / CodeBuddy 的服务条款，自行承担使用风险。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

**GNU Affero General Public License v3.0 only (`AGPL-3.0-only`)**，完整文本见 [LICENSE](LICENSE)。本项目从 2026-11-01 本次迁移起按此许可提供自有新增内容及组合修改版；此前已合法取得的 MIT 授权继续有效。第三方原有版权与许可保留，不将第三方内容统一改署本项目版权。

本许可允许商业使用。分发程序或修改版时须满足对应源码等义务；修改版通过网络向用户提供服务时，须按 AGPL 第 13 条向这些用户显著提供该版本的完整对应源码，不收取源码获取费用。不能以「仅供学习」「禁止商用」等文字增加与 AGPL 冲突的许可限制。上方使用提示和免责声明不改变 LICENSE 授予的权利。

### 网络用户取得对应源码

- 无需管理员登录即可访问 `/legal`（法律声明）、`/legal/license`（许可证）、`/legal/notice`（版权与来源）、`/legal/third-party`（第三方许可）及 `/legal/source`（对应源码归档）。API 响应中的 `Link` 头指向许可证和法律声明；管理界面与解锁窗口均提供入口。反向代理须允许所有远程使用者访问这些路径，不能仅将 `/v1/*` 暴露给 API 用户。
- 将同一次构建的 `source.tar.gz` 安装在服务端可执行文件旁边。服务只读取这一个固定归档，不扫描运行目录、不即时打包账号或配置。缺失时 `/legal/source` 明确返回 503；这表示部署尚未配齐源码，仓库主页或不断变化的 `main` 分支不能代替它。
- 对应源码包含本次实际工作树（含未提交源码）、Go vendor 源码及许可、React 源码与锁文件、npm 生产依赖、构建与安装脚本、文件哈希清单。压缩后的 `frontend/` 不能代替 React 源码。归档不包含凭据、账号池、真实配置、数据库、日志、备份或 `.git`；新增源码文件仍须检查是否含秘密。

### 宿主机发布构建与许可打包

构建和验证继续只在 SSH `tdog` 宿主机执行，先检查连通性并显式加载项目环境。使用隔离源码目录，保留当前工作树的全部必要源码。源码快照没有 `.git` 时，提供由本地 `git ls-files --cached --others --exclude-standard` 生成并经过筛选的文件清单，通过 `SOURCE_FILES_MANIFEST` 指定；从发行源码归档重建时自动使用归档中的 `SOURCE_FILES.txt`。

```bash
source /srv/build-server/env/workbuddy2api.sh
bash scripts/build-release.sh /srv/build-server/artifacts/workbuddy2api/<run-id>
```

脚本按锁文件恢复前端依赖并构建，收集确切依赖版本的许可文本，生成 Go vendor 与 `source.tar.gz`，再构建 server/login/handoff-gateway。缺少依赖许可文本时停止，先核查该依赖再补齐，不静默跳过。输出包含许可证、来源声明、第三方许可、源码、二进制、前端和 SHA256SUMS；测试在发布前单独执行。

GitHub 的验证工作流改为手动触发，并要求配置标记为 `self-hosted, tdog` 的构建机 runner；未配置 runner 时使用上述 SSH 流程，不在 GitHub 托管机器编译。

Docker 仅打包宿主机产物：以输出目录作为构建上下文，使用 `Dockerfile.local` 或 `Dockerfile.gateway`。主 `Dockerfile` 同样改为宿主机产物打包入口，不再在容器内编译。源码归档与二进制必须配套分发和升级；反向代理切换程序时也要同步切换对应归档。手动 `go build` 不会自动生成发布源码，不能直接当作完整发行包。

Go/JavaScript 依赖各按其许可使用，完整文本由发布脚本写入 `LICENSES/dependencies.txt`，并随源码、镜像和 `/legal/third-party` 提供。上游 MIT 文本及 yreidev 的原始 NOTICE 分别保存在 `LICENSES/upstream-MIT.txt`、`LICENSES/yreidev-NOTICE.txt`；其适用材料的署名等要求继续保留。


### 接码平台与任务选择

自动加号支持豪猪和 E码云（D1 同模板）。在「自动加号 → 接码平台」分别配置豪猪凭据与项目，以及 E码云 Token、短信关键词、API 地址、卡类型和省份；两家凭据独立保存，任务切换平台时无需重新输入，配置窗口的凭据留空保留现值。自动模式优先豪猪，已保存的 E码云可作为备用。凭据、平台和关键词修改需等任务结束，验证失败或写盘失败均保留原配置。界面只显示各平台是否已保存凭据，不回传密钥。

启动时没有接码凭据或豪猪登录失败，控制台仍可配置平台；保存 E码云配置后，在任务中选择「仅 E码云」即可使用，无需豪猪配置或再次重启。未配置时显示配置提示并禁用启动按钮。豪猪 UID 监控在未配置豪猪时暂停，不阻断 E码云任务。

配置继续使用 `sms.haozhuma`，默认 `provider=haozhuma`、`mode=auto`。旧配置会在保存时自动迁移为 `credentials.haozhuma`（user/pass/token）和 `credentials.ecode`（token）；旧的 `user/pass/token` 字段保留当前主平台的兼容投影。豪猪 `sid`（如 `52283`）与 `ecode_keyword`（如 `腾讯科技`）必须分开填写。E码云主用和兜底复用 `ecode_base`、`ecode_card_type`、`ecode_province`；旧 `fallback*` 配置仍兼容，停用兜底不删除 `credentials.ecode`。历史上已被覆盖的另一平台凭据无法恢复，需要首次补填；之后切换会保留。默认 E码云 API 为 `https://api.eomsg.com/zc/data.php`，D1 为 `https://api.uomsg.com/zc/data.php`。示例只保留空凭据。

- `auto`：豪猪优先，失败后尝试 E码云；仅配置 E码云时直接使用 E码云。
- `haozhuma` / `ecode`：只用指定平台，未配置时明确拒绝。
- `both`：按取号轮次交替首选，失败时尝试另一侧；需要同时配置两平台，不承诺吞吐翻倍。
- 工作台每批任务显式选择平台，启动后固定；在途号码始终沿用取号时的平台和关键词。旧 API 的全局选择仍保留兼容。
- 豪猪对接码监控任务固定使用豪猪，避免 E码云消费进入豪猪项目预算。
- 在途号码独占使用，供应商重复返号会跳过并退避，不对原持有者执行释放或拉黑。E码云在同一批次内用过的号码不会再次发码；新的批次重新计数。
- 豪猪号码成功收到验证码后立即调用 `addBlacklist`，不再调用 `cancelRecv`；后续验码、注册或本地落盘失败也不改为释放。拉黑失败会记录号码及原项目，任务收尾、下一轮和服务重启后继续重试拉黑。旧版仅含号码数组的遗留账本仍按原规则释放；一键释放不会删除待拉黑记录。
- E码云收码的网络故障、HTTP 408/429/5xx 在原轮询预算内最多连续重试 3 次，按轮询间隔的 1/2/4 倍等待。凭据错误和业务拒绝不重试，手动停止可中断等待。日志区分接口故障与验证码等待超时。
- E码云仅结束本地号码占用，当前适配不调用平台拉黑/释放；界面不提供豪猪的「一键释放」操作，平台回收与计费以供应商规则为准。

供应商请求及实际费用以平台规则为准。仓库、示例与日志不得写入真实 Token。


自动加号主页面的「搜索对接码」可直接打开豪猪对接码列表；尚未选择项目时先搜索项目。入口展示当前项目及轮换池数量，需要有效 H5 会话，任务运行期间禁止修改。选择仅回填草稿，仍需保存高级设置才生效。

自动加号工作台将平台配置、任务执行和豪猪监控分开：两家凭据在各自配置窗口保存；每批只选择一次平台，启动后固定生效，状态接口回显实际目标、来源、平台和分组。任务状态在空闲和临时网络故障后仍持续恢复刷新，账户余额只在手动刷新时查询。

监控参数与项目统一保存，总开关仅修改已保存配置的启停状态，不提交编辑草稿。对接码选择器仅回填草稿，外层保存后才同步账户；同步结果如有部分失败会明确显示，且不会删除本配置未管理的其它对接码。监控借用取号参数期间禁止修改这些参数，避免任务结束时覆盖新配置。

监控项目支持最低价和最高价，仅使用区间内的对接码（包含上下限）。最低价 `min_price` 默认为 0，兼容旧配置，且不能高于最高价。保存后热生效；已锁定的对接码超出区间时，下轮停止使用并重新选择。价格从区间外进入区间时，即使库存未增加也会触发候选。

监控并发 `autoenroll.watch.workers` 在页面统一设置（1–3，默认 1）。每个监控项目可独立设置运营商 `isp` 和省份 `province`：运营商使用豪猪官方代码 1/5/9/14/16（移动/联通/电信/广电/虚拟运营商），多个用逗号分隔；省份使用官方代码，例如北京 11、广东 44。`isp` 缺省或 null 时兼容旧配置，沿用手动优先级；显式空字符串表示不限。明确指定运营商后不退回“不限”，监控对接码失效时也不转向其它对接码或公共池。筛选仅作用于本批任务，不改写手动设置。旧手动界面存下的 2/3 在读取取号时按原界面的联通/电信含义兼容为 5/9；新保存配置仅接受现行官方值。

项目的号码类型 `card_type` 可留空（不限）、设为 `1`（虚拟号）或 `2`（实卡）；实际取号映射到豪猪 `ascription`，省份映射到大小写敏感的 `Province`。字段和枚举依据用户提供的豪猪官方取号参数表及运营商、省份代码表核对，不采用其它 SDK 的相反卡类型定义。运营商和卡类型是两个独立维度。

手动「加号工作台 → 创建加号任务」也可选择「豪猪号码类型」（不限 / 实卡 / 虚拟号）。启动接口 `POST /admin/account/sms/auto-enroll` 的可选字段 `haozhuma_card_type` 使用相同取值，仅对本批豪猪取号生效，不保存为默认值、不改写监控项目，也不覆盖 E码云平台配置。任务状态回显实际采用的类型；仅 E码云任务不能提交非空的豪猪类型筛选。

监控费用以收到短信的号码次数乘触发时单价估算，包括后续注册或落盘失败的已收码尝试，不作为供应商账单。每批尝试上限受剩余预估预算约束；只有正常查询最终无短信、且无本地/接口故障和人工停止的批次才会将对接码标记为暂时不可用。

监控准备下一轮前，会使用原接码客户端及号码所属项目重试清理本服务账本中的遗留号码，再切换到新一轮的客户端。清理期间禁止插入新任务或更换接码配置；释放仍失败时保留账本，显示遗留数量，并按监控间隔自动重试。该过程不会调用账户全量释放接口。恢复后自动清除对应暂停原因，无需反复开关监控。

### 豪猪新版 Web 登录令牌

豪猪增强功能使用新版 Web API（api.veeto.cn/v1）。在「平台配置 → 豪猪增强功能」粘贴 qs_web_token，验证通过后立即生效，并以 AES-GCM 加密保存在 `state_file` 同目录的 `haozhuma-web-token.enc`（文件权限 0600），服务重启后自动恢复。输入框旁有控制台代码和开发者工具手动读取两种教程；Web 令牌与接码 API Token 独立，不回显、不写入配置明文或日志。加密密钥与提示词存储一样，从管理员 API Key（无 API Key 时使用控制台密码）派生，并使用独立用途；备份与恢复时保留原管理员密钥及该加密文件。读取/解密失败会停用增强会话并在界面提示，可检查原密钥或重新保存，不会静默启用其它令牌。保存或清空失败均保留原运行时令牌。

尚无已保存值时，仍可用环境变量 `HAOZHUMA_WEB_TOKEN` 提供本次运行的初始值；界面保存的值优先，显式清空也会持久化，重启后不再恢复环境变量中的旧值。无持久数据路径或稳定管理员密钥的实例只支持内存模式，界面会明确提示；令牌不写入浏览器存储。已在旧进程内存中的令牌需在升级后重新粘贴保存一次。旧 PHPSESSID 不再用于新版接口。令牌失效后重新登录豪猪获取，不假定固定有效期，也不调用 /ping 续期；加密保存不会延长上游令牌有效期。
