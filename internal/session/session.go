// Package session 会话粘性路由：同一会话（conversationId / metadata 键）尽量绑定同一账号。
//
// 设计参考 antigravityProxyGo internal/session（fast-path RLock / 双段分配 / TTL / 持久化），
// 但改为纯内存 + redisstore 异步镜像：
//   - 命中走 RLock 快查（绝大多数请求已绑定）；
//   - 未命中/失效走写锁 re-check 后分配，避免同 key 并发重复分配（TOCTOU 防护）；
//   - 分配优先"空闲账号"（未绑定任何会话的可用号）哈希，其次全池哈希（双段策略）；
//   - LastActive 滚动续期，TTL 过期由后台 GC 或快路径惰性过期清理；
//   - 每次绑定变更 fire-and-forget 镜像到 redisstore（防重启丢粘性）。
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"workbuddy2api/internal/redisstore"
)

// maxStickyKeyLen 限制入站会话键参与哈希的字节数。键会被归一化成固定长度，
// 所以这个上限只是省掉无谓的哈希开销，不承担内存保护职责。
const maxStickyKeyLen = 512

// defaultMaxEntries 绑定表容量上限。TTL 单独并不能把表大小封顶：在一个 TTL 窗口内
// 灌入大量互不相同的 key，每条都是"未过期"的，表会一直涨。
const defaultMaxEntries = 10000

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
}

// Config 路由依赖；Available 返回"可用账号"（healthy 且未占满在途）的有序 uid 列表，
// 由 pool.AvailableUIDs 提供。Store 可为 redisstore.Noop（纯内存）。
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
	// Salt 给会话键的 HMAC 加盐。没有盐时 hashIndex 是公开可算的，
	// 知道账号列表的人就能反推任意 key 会落到哪个 uid，从而抢占别人的会话槽位。
	// 每个部署生成一次并保持不变；换盐会让所有会话重新分配。
	Salt []byte
	// MaxEntries 绑定表上限；<=0 取 defaultMaxEntries。
	MaxEntries int
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	cfg     Config
	stop    chan struct{}
}

// New 构建路由器。若 cfg.Store 为 nil 则用 Noop（纯内存）；cfg.Available 为 nil 视为空池。
// TTL/GCInterval 非正取默认（30m / 5m）——main 从 config 解析后传入，这里兜底。
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = defaultMaxEntries
	}
	if len(cfg.Salt) == 0 {
		// 没有配置盐时退化为不带密钥的 SHA-256：仍然把键归一化成定长，
		// 内存有界，但不提供抗预计算能力。main 会生成随机盐并告警。
		log.Printf("[session] sticky salt is empty; session keys are not keyed (set session_sticky.salt)")
	}
	return &Router{entries: map[string]entry{}, cfg: cfg}
}

// normalizeKey 把客户端提供的会话键转成实际用作 map/redis key 的定长标识。
//
// 两个作用：
//  1. 定长——10MB 的 conversation_id 和 10 字节的一样只占 32 字节，客户端的
//     无界输入无法再直接放大内存；
//  2. 不可预测——HMAC 加盐后，知道账号列表也无法反推某个 key 会落到哪个 uid。
func (r *Router) normalizeKey(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > maxStickyKeyLen {
		raw = raw[:maxStickyKeyLen]
	}
	mac := hmac.New(sha256.New, r.cfg.Salt)
	mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}

// evictLocked 在写入前保证表不超上限。调用方必须已持有写锁。
// 淘汰最久未活跃的条目（与 GC 的过期语义一致，只是提前触发）。
func (r *Router) evictLocked(now time.Time) {
	if r.cfg.MaxEntries <= 0 || len(r.entries) < r.cfg.MaxEntries {
		return
	}
	// 一次淘汰 1/8，避免每个请求都在这里做 O(n) 扫描。
	target := len(r.entries) - r.cfg.MaxEntries + r.cfg.MaxEntries/8 + 1
	type kv struct {
		key string
		at  time.Time
	}
	all := make([]kv, 0, len(r.entries))
	for k, e := range r.entries {
		all = append(all, kv{k, e.lastActive})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for i := 0; i < target && i < len(all); i++ {
		delete(r.entries, all[i].key)
		r.cfg.Store.DelBind(all[i].key)
	}
}

// StartGC 启动后台 GC goroutine（幂等）。进程退出时调 StopGC。
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()

	go func(stop <-chan struct{}) {
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}(stop)
}

// StopGC 停止后台 GC（幂等）。
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore 启动时从 redisstore 恢复绑定（内存覆盖本地，读操作仅此处发生）。
// 已有本地绑定被保留——Redis 仅为恢复备份，本地一旦建立即为权威。
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	loaded := 0
	for key, uid := range binds {
		if _, exists := r.entries[key]; exists {
			continue
		}
		r.entries[key] = entry{uid: uid, lastActive: now}
		loaded++
	}
	r.mu.Unlock()
	if loaded > 0 {
		log.Printf("[session] 从 Redis 恢复 %d 条粘性会话绑定", loaded)
	}
}

// Resolve 返回会话 key 应绑定的账号 uid，ok=false 表示当前无可用账号。
// 命中且账号可用 → 滚动 lastActive 并直接返回；否则（lazy 异常情况）走重新分配。
//
// 边界归一化：key 在这里一次性转成定长 HMAC。内部（touch / gcOnce /
// LoadFromStore）拿到的都是已归一化的键，**不要**再归一化一次——对归一化结果
// 再做一次 HMAC 会得到另一个值，快路径写进去的键下次就查不到了，粘性会话会
// 在"看起来正常路由"的同时每请求重新分配。
func (r *Router) Resolve(key string) (string, bool) {
	key = r.normalizeKey(key)
	if key == "" {
		return "", false
	}
	now := time.Now()
	available := r.availableSet()

	// ── Fast path: RLock 快查 ──────────────────────────────
	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) {
		if available[e.uid] {
			r.touch(key, e.uid, now)
			return e.uid, true
		}
		// 绑定号已冷却/占满 → 失效，落入慢路径重分配。
	}

	// ── Slow path: 写锁 re-check 后分配 ────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// re-check：并发同 key 可能已被其他 goroutine 分配好。
	if e2, found2 := r.entries[key]; found2 && !expired(e2, now, r.cfg.TTL) {
		if available[e2.uid] {
			r.entries[key] = entry{uid: e2.uid, lastActive: now}
			return e2.uid, true
		}
		delete(r.entries, key) // 失效：清掉再分配
	}

	uids := r.availableSlice()
	if len(uids) == 0 {
		return "", false
	}

	// 双段策略：优先"空闲账号"（未被任何会话绑定的可用号），其次全池。
	bound := map[string]bool{}
	for _, v := range r.entries {
		bound[v.uid] = true
	}
	var idle []string
	for _, u := range uids {
		if !bound[u] {
			idle = append(idle, u)
		}
	}
	pool2 := idle
	if len(pool2) == 0 {
		pool2 = uids
	}
	uid := pool2[hashIndex(key, len(pool2))]

	// 写入前先保证表不超上限：TTL 只能在 GC 周期内清理过期项，
	// 无法阻止"一个 TTL 窗口内灌入大量不同 key"造成的无界增长。
	r.evictLocked(now)
	prev, existed := r.entries[key]
	r.entries[key] = entry{uid: uid, lastActive: now}
	if existed && prev.uid != uid {
		r.cfg.Store.DelBind(key)
	}
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	return uid, true
}

// touch 滚动 lastActive 并异步镜像（只在快路径命中时写最后一次）。
func (r *Router) touch(key, uid string, now time.Time) {
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值），并异步镜像到 redisstore。
// 供"粘性跟随最终成功号"用：请求成功返回前，把会话重绑到实际成功的账号，让多轮对话下一跳稳定
// 收敛到"对该会话持续成功的号"（对齐 antigravity 语义）。空 key 直接返回（无会话则不绑）。
func (r *Router) Bind(key, uid string) {
	key = r.normalizeKey(key)
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.evictLocked(now)
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Unbind 解除会话绑定（请求失败时调用，让该会话下次重新分配）。返回是否存在。
func (r *Router) Unbind(key string) bool {
	key = r.normalizeKey(key)
	if key == "" {
		return false
	}
	r.mu.Lock()
	_, found := r.entries[key]
	if found {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	if found {
		r.cfg.Store.DelBind(key)
	}
	return found
}

// Count 返回当前绑定数（供 /status 观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce 清理 TTL 过期的绑定，并镜像删除。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	var expiredKeys []string
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			expiredKeys = append(expiredKeys, key)
		}
	}
	for _, key := range expiredKeys {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	for _, key := range expiredKeys {
		r.cfg.Store.DelBind(key)
	}
	return len(expiredKeys)
}

// availableSet 把 Available() 的有序列表转集合（快路径命中校验用）。
func (r *Router) availableSet() map[string]bool {
	uids := r.availableSlice()
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// availableSlice 安全调用 Available（nil 函数视空池）。
func (r *Router) availableSlice() []string {
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a 哈希取模（antigravity 双段分配的稳定散列）。
func hashIndex(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// ExtractKey 从请求体提取会话键；按优先级依次尝试，找不到返回空串（绝不失败）。
//  1. metadata.conversation_id
//  2. conversation_id
//  3. conversation（Responses API 的会话键）
//  4. prompt_cache_key（Responses API 的显式缓存分区键）
//  5. metadata.user_id
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["conversation"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		return strOrEmpty(meta["user_id"])
	}
	return ""
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
