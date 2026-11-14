# 2026-11-14：dogyun overlap 发布与故障复盘

用户决定：保留国内版仍返回的 5 项成长任务，只有 `RichMeow_Chat`、`Library_read`、`first_buddy` 自动执行；`wb_wechat_oa_subscribe_task`、`skill_share` 保持手动。部署后，后续常规升级直接使用 overlap，先切新请求，再等待旧流自然结束。

## 最终确认的生产基线

以下是当日快照，不代替下次发布时的只读核对。

| 项目 | 验收值 |
| --- | --- |
| 主机 / 公网入口 | `dogyun` / `https://wb.gaozaiya.cc` |
| 活动 release | `growth-current-routefix-20261114` |
| 业务镜像 ID | `sha256:1e83cb4871567997095ce48e07f78a5556e9df82ffcd98a4677aaac0560c4eb8` |
| 业务容器 | `wk-release-growth-current-routefix-20261114` |
| 业务 / gateway 模式 | 均为 `overlap`，协议 1，后台唯一 owner |
| 当前持久协调目录 | 宿主 `/opt/wk/data/upgrade-members-routefix-20261114`；容器 `/app/data/upgrade-members-routefix-20261114` |
| 当前协调域 | `70d230270258628b270a340fd32b5aa7` |
| 写锁 | 宿主 `/run/wb2api-upgrade/writer.lock`；容器 `/upgrade-run/writer.lock` |
| 保留的历史域 | `29840bb8c82d392edbac7d2b7666642f`，历史目录 `/opt/wk/data/upgrade-members` |
| 账号 / 凭据文档 | 41 / 41，账号集合校验一致，全部健康 |

数据库、凭据、配置与状态目录保留；没有恢复整库。切换时的 41 行 legacy 账号状态及历史共享域记录，在上线后做聚合摘要比对，均未被覆盖。新域从正常退出的当前 serial 状态 bootstrap，不从过期历史共享域恢复。

## 踩坑、原因和修正

### 1. 代码支持 overlap，不代表线上已经启用

最初生产业务仍是 serial，稳定 gateway 也需要迁移。首次迁移必须单独处理，不能把它当作已完成的 overlap 发布。现已完成首次迁移；以后复用当前 gateway、写锁和协调目录，使用原控制器的 `--mode overlap` 分支，不再执行“先 hold 全部推理 → 排空旧请求 → 启动新业务”的历史步骤。

### 2. JSONB 拒绝 NUL 路由键，造成账号显示为空

原 `routeLeaseKey` 用 NUL 拼接 Tenant / Session / Prefix。真实会话路由进入共享 PostgreSQL JSONB 后触发 `SQLSTATE 22P05` / `unsupported Unicode escape sequence`，共享池永久 fence；随后 health/ready 失败、状态页可能显示零账号。这不是账号文件被删除的证据。

修正：三个字段分别做 URL-safe Base64 编码，再用 `.` 分隔，保持身份边界。没有弱化共享故障时的拒绝接单行为。真实 PG 回归先证实旧键失败，再验证 41 假账号、两个共享 pool、名额/owner/释放语义。

原失败后曾恢复到 serial，确认当时 41 个账号、41 份凭据、41 行 legacy 状态及 41 个历史共享账号均存在，旧域保留。这段恢复经历不构成可以在任意共享写入后无损回退 serial 的通用证明。

### 3. 只测暖态会漏掉启动采集的长锁

初版修复通过暖态长稳，但 ready 后立即运行 4 路 SSE 与 `/status`，后者仍可能超过原 20 秒 socket 超时。

原因：`collection.Manager.Submit` 在 manager 全局锁内调用 UIDs/逐资源 Eligible；共享 pool 的读取也会获取独占锁并执行 PG/JSON 操作，41 账号 × 5 资源放大了等待。抓到的栈支持长锁争用，未发现该路径中的循环死锁。

修正：账号展开、校验、去重与每 UID 一次的资格查询在锁外准备；复锁后按当前队列重查 closed/active、容量、版本、合并关系，再持久化/发布；执行前仍重查资格。保留取消、generation、首次保存失败不接单等原有语义。

新增 channel/race 回归，并以原实现做负对照，确认旧实现会被测试检出。冷启动测试不得先调用 `/status` 或先发一条 SSE 预热，也不得放宽原超时掩盖问题。

### 4. 新建协调目录会与原写锁的域引用冲突

串行恢复后，旧 `writer.lock.overlap-domain` 仍指向历史目录。直接给候选一个新目录时，`JoinOverlap` 以 `overlap domain mismatch; use the same coordination-dir and writer lock` 拒绝启动；此失败发生在打开 PG 之前。候选尚未 bootstrap，确认旧进程正常退出、候选已退出、无其他业务写入者后，才恢复原 serial 接流。

该次尝试产生约 80 秒排队/恢复窗口。修正后的**一次性离线迁移**：

1. 原 serial 正常退出，所有自动重启受控，持有部署守卫。
2. 保持原 writer 文件及 inode，取得 EX/NB 写锁，再取得 `.overlap-init`、两个目录的 `domain-init.lock` 及成员、owner、proxy 锁；任一冲突即拒绝。
3. 核对实际挂载映射、旧 reference/两个 domain.json，确认新 PG 域为空。
4. 独立 0600 文件原字节保存旧 reference 与摘要；只原子更新 reference，保留属主、权限并 fsync，不改旧目录和旧 PG 域。
5. 启动准确候选；先确认 mode、domain、owner、ready 和真实回环入口，再放行。

隔离实测历史 overlap → serial（更新余额、计数和禁用状态）→ fresh 域，验证 41 账号最新状态、当前 serial Cookie、sticky 绑定、原 inode、旧 PG 不变，以及新域重启；10 类拒绝负例通过。**此 helper 只用于本次首次迁移，日常 overlap 发布不得再切域。**

### 5. 共享 bootstrap 后不能直接恢复 serial

新 owner 在 ready 之前即可激活后台；gateway hold 不代表没有写入。共享 pool 关闭 legacy persister，最终 flush 只提交共享 PG，不会把最新余额/消耗/冷却等状态导回 legacy 表。

因此，共享域初始化或开始写入后，故障恢复应使用**当前同一域的兼容 overlap 版本**。禁止直接启动旧 serial 或复用串行运行前的旧域；确需改模式时，要单独验证当前权威状态的迁移。账号数和 UID 摘要相同不足以证明余额等状态未丢失。历史镜像里的旧 NUL 路由实现不能作为 overlap 回滚版本。

### 6. 后台进程需要真正独立的存活边界

`start_new_session=True` 不能让监控脱离 systemd cgroup。首轮失败时监控随部署单元退出，只留下两个健康样本，不能用“0 个错误样本”证明恢复窗口无中断。

修正：监控用独立 systemd 单元，有限 RuntimeMaxSec 和停止标记；失败后仍继续采样。部署守卫覆盖异常处理，FD 继承到 controller/helper；超时终止并等待整个辅助进程组，避免父级已退出、helper 仍修改域引用。隔离测试验证父级死亡仍保持互斥、killpg 后锁释放且无延迟孤儿写入。

### 7. 构建、传输和排障细节

- 项目构建主机为 `.build-test.json` 指定的 `tyy2`，不是旧文档中的 `tdog`。先检查连接和数据盘；env 脚本只 source，不输出全文。
- `dogyun` 不支持 SFTP 子系统，使用 `scp -O`。Windows 原生文本管道不能传二进制 tar；本次用 Node 字节流经两条 SSH 转发，端到端校验长度与 SHA256。
- 镜像传输较慢时可使用固定不可变 base ID 的 COPY-only 增量包，包含新 binary、完整 source 与许可证；不在生产编译，不重新打包未变化的 gateway。
- 不为加速传输额外探查私钥或开未经认证的公网制品服务。
- 自动审批曾拒绝原样输出生产 handoff 日志；后改为状态白名单、固定错误分类及 SQLSTATE，不输出完整配置、环境或日志。

## 验收结果与证据

| 检查 | 结果 |
| --- | --- |
| 完整 collection/collectionservice、锁回归/race/负对照、真实 PG pool/coordination、server 指定回归 | 通过 |
| ready 后立即 4 路 SSE 的冷启动 | 180.734 秒；686 SSE，144 health，144 status，146 PG 快照 |
| 新 PG/新进程独立冷启动重复 | 60.892 秒；164 SSE，41 health，41 status，43 PG 快照 |
| 暖态真实路由长稳 | 181.106 秒；693 SSE，146 health，146 status，149 PG 快照 |
| serial→overlap、旧 SSE 与新请求并行、owner/session、10 项控制器回归 | 通过 |
| 最终生产连续观察 | 305.02 秒，160 个 health 样本，始终 41/41 健康 |
| 最终生产监控 | 578 样本，0 HTTP 5xx；1 次公网 URLError；最大排队样本约 16.5 秒 |

冷/暖夹具未记录逐次 health/status 的精确最大耗时；只能确认原 20 秒超时未触发。最终监控的 16.5 秒不能推作全部用户请求的最大延迟；历史首次失败也不能由最终成功采样覆盖。

- 部署记录：`dogyun:/opt/wk/releases/20261114-growth-routefix/deployment-report.json`；本地 `tmp/deploy-growth-routefix-20261114/deployment-report.json`。
- 完整许可源码 SHA256：`1a400593cdafb3eea5e672a173ff702feb38f74ab711dee152da841ae3369c0f`。
- 业务 binary SHA256：`0a8cb75ec4e29118cb9c2c1ccfb81f4f7eb54e06f1dd6d416f4d35cb5bee04f7`。
- 构建/长稳证据：`tyy2:/srv/build-server/{artifacts,logs}/workbuddy2api/overlap-routekey-hotfix-20261114-r2/`。
- 一次性迁移验证：同目录体系下 `overlap-routekey-hotfix-20261114-r3/verification.json`，SHA256 `861b5ef98c547a4501abe542fdd966ddcaaafec922c7d5c28a88bd54ba02f703`。

后续按 [当前部署流程](../../DEPLOY.md) 发布；发布容器、镜像、端口、基线账号数每次重新核对。
