

## 快速开始

### 1. 克隆 & 配置

```bash
git clone https://github.com/Sliverkiss/workbuddy2api.git
cd workbuddy2api
cp config.example.json config.json
# 编辑 config.json，把 api_key 改成一个你自己生成的强随机值
# 例：openssl rand -hex 32
```

> `config.example.json` 里是**占位符**，服务会拒绝用它们启动。请务必替换成真实值。
> 也可以完全不写配置文件，直接用环境变量：`WB2A_API_KEY=... docker compose up -d --build`。

### 2. 添加账号

```bash
./login.sh
# 打开浏览器登录 → 按 y → 自动落盘 auths/ → 重启容器

# 添加海外版账号（中国区账号仍会保留）
./login.sh global
```

### 3. 启动服务

```bash
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

`session_sticky.salt` 是给会话键做 HMAC 的密钥（hex，至少 16 字节）。**生产环境建议显式设置**：留空时启动会生成一个随机盐并打印告警，此时粘性绑定在重启后全部重新分配，且会话键在缺少密钥的情况下可被离线预计算。生成方式：`openssl rand -hex 32`。

`session_sticky.max_entries` 是绑定表上限（默认 10000）。TTL 只能回收"已经过期"的条目，无法限制单个 TTL 窗口内涌入的大量新键，因此需要这个显式上限；到达上限时会淘汰最旧的一批绑定。

### 缓存感知路由

启用 `session_sticky` 时，`cache_routing.enabled` 默认开启。账号暂时满载不会删除会话绑定；请求先在有上限的队列中等待，有相近前缀缓存的备用账号更快时可临时借用。等待超时或队列已满后允许普通选号以维持可用性，临时借用不会覆盖主绑定。账号失效或当前模型冷却时才通过版本检查迁移绑定；旧并发请求的完成不会把新绑定改回去。

短期活跃保护与 30 分钟会话绑定独立：成功请求建立 5–15 秒的保护期，根据成功请求间隔调整；已有热会话的请求运行时保护继续有效，失败、取消或排队本身不续租。记录按鉴权身份、会话与规范化开场前缀隔离；更换开场内容不能继承旧前缀的优先权。保护记录最多与 `max_prefix_entries` 相同数量，不保存正文或密钥。

新会话优先选择预计占用压力较小、近期热会话较少的账号。在每账号并发上限为 3 时，默认软预留 1 个名额给近期会话或具有相关前缀缓存的请求。只有存在另一授权范围内、模型健康且有空闲名额的账号时才阻止无关新请求使用预留名额；全池拥堵、替代账号冷却或授权仅剩本账号时允许借用。检查与占名额在同一锁内，排队不能绕过检查。已开始的请求不抢占，预留由所有近期会话共享，不保证每个会话立即获得名额；并发上限为 0 或 1 时不预留。

队列默认按可信鉴权范围轮转，再在该范围内按会话轮转。缓存相关请求获得有限优先权，连续优先最多 2 次后让普通请求竞争；等待达到 `queue_aging` 的请求按最早入队优先，避免持续被新请求超越。身份为空的旧调用保留 FIFO；普通选号不能插队。这里是按请求轮转的公平策略，不能保证不同 token 长度的请求具有相同计算份额。网关若把多个下游用户汇聚到同一上游鉴权密钥，第一层只能识别该共同鉴权范围，不把客户端自报 `user_id` 当可信用户身份。

优先读取 `metadata.conversation_id`、`conversation_id`、`conversation`、`prompt_cache_key`，以及 `X-WorkBuddy-Session-ID` / `X-Session-ID` / `X-Conversation-ID` 请求头。标识需在同一对话中保持稳定，并由网关实际转发。缓存路由开启时，不把 `metadata.user_id` 单独当作会话标识，避免把同一用户的不同任务合并。绑定按鉴权密钥身份、分组与规范化模型隔离。升级后的首次请求会在权限和模型健康校验通过时沿用旧版显式标识对应的账号，并建立新的隔离绑定。

缺少显式标识时，根据规范化后的 system、tools 与第一条 user 消息生成会话提示，再校验完整历史的前缀摘要选择账号。该提示不是经过认证的会话 ID；相同开场或历史压缩仍可能使提示重复或变化。候选索引只保存带秘密盐的摘要和使用量，最多保存 256 个自适应前缀检查点，不保存请求正文。前缀索引是进程内的近似记录，并不等同于上游真实 KV 状态。

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

**注意**：`cooldown.hard_credit` / `cooldown.err_threshold` / `cooldown.err_cooldown` 三个历史键已退役。硬冷却固定为**次日 04:00**（本地时区，`CooldownUntilTomorrow4AM`），连续错误语义并入熔断器（`pool.breaker_threshold` 触发指数退避）。旧配置中的这些键因 JSON 未知字段被自然忽略，不报错。

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
- **会话粘性路由**：同一 `metadata.conversation_id`/`conversation_id`/`metadata.user_id` 尽量绑定同一账号，TTL 滚动续期；请求失败自动解绑回落轮换，请求成功后会话绑定**跟随最终成功号**。
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
| `./login.sh` | OAuth 登录，落盘 auth 文件 |
| `./credit.sh` | 积分日报（美化输出） |
| `./credit.sh -json` | 积分原始 JSON |
| `./signin.sh` | 批量签到（遍历 auths/ 下所有账号） |

## API 端点

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | Bearer | OpenAI 兼容聊天补全（流式/非流式） |
| `POST /v1/responses` | Bearer | Responses API 适配（流式/非流式、工具调用、`previous_response_id`） |
| `GET /v1/models` | Bearer | 模型列表（动态拉取 + 静态兜底） |
| `GET /status` | Bearer | 账号状态汇总（total/healthy/cooling/disabled + 每账号详情；详情含模型级 `model_cooldowns`） |
| `GET /requests?limit=50` | Bearer | 最近请求日志（`limit` 默认 50、上限 500；不含提示词和响应正文） |
| `POST /admin/credits/refresh` | 前端会话/Bearer | 异步刷新所有账号的上游积分明细，不执行签到 |
| `POST /admin/account/{uid}/enable` | 前端会话/Bearer | 手动启用账号并清除禁用/冷却状态 |
| `POST /admin/account/{uid}/disable` | 前端会话/Bearer | 手动禁用账号，停止新请求使用 |
| `DELETE /admin/account/{uid}` | 前端会话/Bearer | 删除账号池记录及对应授权文件 |
| `POST /admin/account/url?region=cn|global` | 前端会话/Bearer | 生成对应区域 OAuth 授权链接 |
| `POST /admin/account/poll?region=cn|global` | 前端会话/Bearer | 轮询并保存对应区域授权结果 |
| `GET /healthz` | 无 | 健康检查（无健康账号时 503） |

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

本项目仅供学习和研究使用。使用者需遵守 WorkBuddy / CodeBuddy 的服务条款，自行承担使用风险。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

MIT


### 接码平台与任务选择

自动加号支持豪猪和 E码云（D1 同模板）。在「自动加号 → 接码平台配置」中配置主平台、Token、E码云短信关键词、API 地址、卡类型和省份；豪猪主用时可挂载 E码云兜底。凭据、平台和关键词修改需等任务结束，验证失败或写盘失败均保留原配置。

启动时没有接码凭据或豪猪登录失败，控制台仍可配置平台；选择「E码云主用」验证并保存后立即启用，无需豪猪配置或再次重启。未配置时显示配置提示并禁用启动按钮。豪猪 UID 监控在未配置豪猪时暂停，不阻断 E码云任务。

配置继续使用 `sms.haozhuma`，默认 `provider=haozhuma`、`mode=auto`，兼容原有豪猪配置。豪猪 `sid`（如 `52283`）与 `ecode_keyword`（如 `腾讯科技`）必须分开填写。E码云主用使用主 `token`；豪猪主用的 E码云凭据使用 `fallback`，地址、卡类型、省份分别使用 `fallback_base`、`fallback_card_type`、`fallback_province`。默认 E码云 API 为 `https://api.eomsg.com/zc/data.php`，D1 为 `https://api.uomsg.com/zc/data.php`。示例只保留空凭据。

- `auto`：豪猪优先，失败后尝试 E码云；仅配置 E码云时直接使用 E码云。
- `haozhuma` / `ecode`：只用指定平台，未配置时明确拒绝。
- `both`：按取号轮次交替首选，失败时尝试另一侧；需要同时配置两平台，不承诺吞吐翻倍。
- 「本次任务平台」只覆盖当前批次；跟随全局时下一次取号读取最新全局选择。在途号码始终沿用取号时的平台和关键词。
- 豪猪对接码监控任务固定使用豪猪，避免 E码云消费进入豪猪项目预算。
- 在途号码独占使用，供应商重复返号会跳过并退避，不对原持有者执行释放或拉黑。E码云在同一批次内用过的号码不会再次发码；新的批次重新计数。
- E码云收码的网络故障、HTTP 408/429/5xx 在原轮询预算内最多连续重试 3 次，按轮询间隔的 1/2/4 倍等待。凭据错误和业务拒绝不重试，手动停止可中断等待。日志区分接口故障与验证码等待超时。
- E码云仅结束本地号码占用，当前适配不调用平台拉黑/释放；界面不提供豪猪的「一键释放」操作，平台回收与计费以供应商规则为准。

供应商请求及实际费用以平台规则为准。仓库、示例与日志不得写入真实 Token。
