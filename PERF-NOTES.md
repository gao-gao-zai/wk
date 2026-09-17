# 性能剖面结论（2026-09-18，950 rps 合成负载）

## 测量方法

- 驱动器：`cmd/server/profile_test.go`（`WK_PROFILE=1` 启动，100 并发 × 60s，
  mock 上游 4 帧 SSE、每帧 25ms，metrics 不落盘以隔离被测路径）
- 剖面：CPU 20s（`benchmarks/cpu-100c.pprof`）、heap/mutex 快照
  （`cmd/server/testdata/pprof/`）
- 复现：
  ```powershell
  $env:WK_PROFILE='1'
  go test ./cmd/server/ -run 'TestProxyProfile$' -v -timeout 10m   # 终端 1
  go tool pprof -seconds 20 http://127.0.0.1:6061/debug/pprof/profile  # 终端 2
  ```

## CPU 剖面（总样本 48.67s / 20s ≈ 243%，即 2.4 核饱和）

| 热点 | 占比 | 归因 |
|---|---|---|
| `runtime.cgocall` → `WSASend`/`WSARecv` | **36.9%** | Windows 上每个 socket 读写都过 cgo。其中 ~28% 是 `response.Flush`（SSE 逐帧 Flush → 每帧一次 syscall）；~6% 是读方向 |
| `encoding/json`（Unmarshal 家族） | ~9% | 每请求多次反序列化：请求体校验（map）、会话键提取、SSE 每帧 map 解码 |
| `regexp.tryBacktrack`（`hasFingerprint`） | **3.1%** | `sanitizeHdrRe.MatchString` 兜底在**每个 text part**上跑，即使 `strings.Contains` 预检全部不中也要跑一次正则 |
| 调度器（semawakeup/osyield） | ~6% | 高并发 + 频繁小写入的固有代价 |

## Heap 剖面（alloc_objects，4880 万对象 / 60s）

| 热点 | 占比 | 归因 |
|---|---|---|
| `reflect.unsafe_New` + json decode | ~35%（Unmarshal 累计） | SSE 每帧 `json.Unmarshal` 到 `map[string]any` 的代价：map/key/value 全是反射分配 |
| `normalizeFrameWithID` | 5.9% | 每帧重构造规范化 map |
| `textproto.readMIMEHeader`/`MIMEHeader.Set`/`Header.Clone` | ~12% | HTTP 头处理（代理双侧各一套） |

## Mutex 剖面

**无锁竞争**（0 delay）——pool 选号锁、metrics 锁在 100 并发下都不构成瓶颈。

## 结论与建议（按 ROI 排序）

1. ~~**`hasFingerprint` 的正则兜底先移到 Contains 命中之后**~~ **✅ 已完成（2026-09-18）**
   实测（`benchmarks/sanitize-{baseline,after}.txt`，benchstat，p=0.002 n=6）：
   - 普通短消息：1250ns → 133ns（**-89.3%**）
   - 22KB 长上下文无指纹：628µs → 16µs（**-97.5%**，吞吐 34MB/s → 1340MB/s）
   - 含指纹文本：不变（本来就走快速路径）
   改法：`sanitize.go` hasFingerprint 增加"nthropic"小写锚点二级预筛——
   header 键名的两种现实形态（全小写 / Title-Case）里该子串恒为小写，
   锚点不中直接跳过正则；任意混排变体仍落正则，语义不变。
   脱敏语义回归：`internal/upstream` 全部 14 个 sanitize 测试通过，
   含大小写变体用例（TestBillingHeaderCaseInsensitive）。

2. **SSE 逐帧 Flush 合并**（~28% CPU 在 Windows 上）
   `response.Flush` 每帧触发 WSASend syscall。可考虑按 N 字节或 T 毫秒
   合并后再 Flush（需权衡首字延迟——TTFB 帧仍应立即 Flush）。
   Linux 生产上 cgo 开销不同，此项收益需在目标平台复测。

3. **SSE 帧解码避免 `map[string]any`**（~35% 分配 + ~9% CPU）
   `logging.go` 注释里已有前例（"每帧三次 unmarshal"已被改成一次）。
   下一步是把剩余的那一次换成 `json.Decoder` + 定向结构体，或仅在
   末帧（含 usage）时全量解码、中间帧只提 content 长度。改动面大，
   建议作为独立一轮，用 `internal/pool/bench_test.go` 的模式先建微基准。

4. **state.json 序列化：不建议现在改**（评估结论，2026-09-18）
   基准数据（`benchmarks/pool-baseline.txt`，46 账号）：
   - `MarshalIndent`：~220-275 µs/次，~50 KB，279 allocs
   - `Marshal`（compact）：~130-157 µs/次，~31 KB，278 allocs
   - 落盘频率：flusher 每 5s 且仅 dirty 时一次；`saveLocked` 持锁执行
   收益评估：省 ~40% 序列化时间和 ~40% 字节，但绝对量是"每 5 秒
   100µs + 19KB"——对请求路径零影响（不在请求路径上）。换 compact
   会牺牲 state.json 的人工可读性（运维需要能直接 cat/编辑它）。
   **结论：收益太小，可读性损失不值得。保持 MarshalIndent。**
   如果未来账号规模 ×100（4600 账号）或 flush 频率被调高，再重新评估。

5. **不要动的**：pool.Pick（9µs/次，46 账号）、SQLite metrics（本测量已
   隔离；生产瓶颈在磁盘不在 CPU）、锁（无竞争）、state.json 序列化（见 4）。

## 已修复（本轮）

- `haozhuma`/`smslogin.TwoCaptchaSolver`：`http.Client{}` 默认 Transport
  `MaxIdleConnsPerHost=2` → 克隆 DefaultTransport 并放宽（轮询场景
  反复重建连接的问题）。
- `upstream.hasFingerprint` 二级预筛（见"结论 1"）：普通消息 -89%，
  长上下文 -97.5%，p=0.002。

## 基线

- `benchmarks/pool-baseline.txt`：internal/pool 五项基准 ×6 次
- `benchmarks/cpu-100c.pprof`：100 并发 CPU 剖面
- `cmd/server/testdata/pprof/`：heap + mutex 快照（三轮）
