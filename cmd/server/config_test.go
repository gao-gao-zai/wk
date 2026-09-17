package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 流式超时默认值：不限总时长（否则长回答仍会被整请求超时掐断，正是本次改造要解决的问题），
// 但空闲窗口必须非零，否则上游卡住时没有任何保护。
func TestStreamTimeoutDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 120 {
		t.Errorf("non-stream timeout=%ds want 120", c.Upstream.TimeoutSeconds)
	}
	if c.StreamTimeoutDur != 0 {
		t.Errorf("StreamTimeoutDur=%v want 0 (unlimited by default)", c.StreamTimeoutDur)
	}
	if c.StreamIdleDur != 120*time.Second {
		t.Errorf("StreamIdleDur=%v want 120s", c.StreamIdleDur)
	}
}

// 未设置/写 0 = 用默认 120s；显式 -1 = 关闭空闲检查。两者必须区分，
// 否则漏配一个 0 就会静默关掉唯一的流式保护。
func TestStreamIdleSecondsZeroVsDisabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		secs int
		want time.Duration
	}{
		{"unset/zero uses default", 0, 120 * time.Second},
		{"explicit value", 45, 45 * time.Second},
		{"negative disables", -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Upstream.StreamIdleSeconds = tc.secs
			if err := c.normalize(); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if c.StreamIdleDur != tc.want {
				t.Errorf("secs=%d -> %v want %v", tc.secs, c.StreamIdleDur, tc.want)
			}
		})
	}
}

// 流式总上限可显式开启（兜底跑飞的流），负数/0 都视为不限。
func TestStreamTimeoutSecondsNormalization(t *testing.T) {
	for _, tc := range []struct {
		secs int
		want time.Duration
	}{
		{600, 10 * time.Minute},
		{0, 0},
		{-5, 0},
	} {
		c := Default()
		c.Upstream.StreamTimeoutSeconds = tc.secs
		if err := c.normalize(); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if c.StreamTimeoutDur != tc.want {
			t.Errorf("secs=%d -> %v want %v", tc.secs, c.StreamTimeoutDur, tc.want)
		}
	}
}

func TestStreamTimeoutEnvOverrides(t *testing.T) {
	t.Setenv("WB2A_STREAM_TIMEOUT_SECONDS", "600")
	t.Setenv("WB2A_STREAM_IDLE_SECONDS", "-1")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.StreamTimeoutDur != 10*time.Minute {
		t.Errorf("StreamTimeoutDur=%v want 10m", c.StreamTimeoutDur)
	}
	// -1 必须能通过 env 传进来（Atoi 接受负号），否则"关闭"无法远程配置。
	if c.StreamIdleDur != 0 {
		t.Errorf("StreamIdleDur=%v want 0 (disabled via env)", c.StreamIdleDur)
	}
}

func TestStreamTimeoutFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":90,"stream_timeout_seconds":300,"stream_idle_seconds":30}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.TimeoutSeconds != 90 || c.StreamTimeoutDur != 300*time.Second || c.StreamIdleDur != 30*time.Second {
		t.Fatalf("timeouts=%d/%v/%v", c.Upstream.TimeoutSeconds, c.StreamTimeoutDur, c.StreamIdleDur)
	}
}

func TestDefault(t *testing.T) {
	c := Default()
	// 默认只监听回环：":7863" 会 bind 所有网卡，任何鉴权缺陷都会被放大成远程可达。
	// 需要对外暴露（含容器，见 Dockerfile 的 WB2A_LISTEN）时必须显式覆盖。
	if c.Listen != "127.0.0.1:7863" {
		t.Errorf("listen=%s", c.Listen)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.SoftRateDur.Seconds() != 60 {
		t.Errorf("soft=%v", c.SoftRateDur)
	}
	if c.Postgres.MaxOpenConns != 16 || c.Postgres.MaxIdleConns != 8 {
		t.Errorf("postgres pool defaults=%+v", c.Postgres)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k","region":"cn"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestMixedRegionConfig(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"region":"mixed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load mixed region: %v", err)
	}
	if c.Region != "all" {
		t.Fatalf("region=%q want all", c.Region)
	}

	if err := os.WriteFile(fp, []byte(`{"region":"all"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(fp)
	if err != nil || c.Region != "all" {
		t.Fatalf("load all region: region=%q err=%v", c.Region, err)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

// TestEnvWiringForSecurityFields covers the newer env vars. They are documented
// in .env.example and README, so an unwired variable would silently leave the
// documented setting without effect.
func TestEnvWiringForSecurityFields(t *testing.T) {
	t.Setenv("WB2A_FRONTEND_DIR", "/srv/console")
	t.Setenv("WB2A_SESSION_SALT", "aabbccddeeff00112233445566778899")
	t.Setenv("WB2A_TRUSTED_PROXIES", "10.0.0.1, 172.17.0.0/16 ,")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.FrontendDir != "/srv/console" {
		t.Errorf("FrontendDir=%q, want /srv/console", c.FrontendDir)
	}
	if c.SessionSticky.Salt != "aabbccddeeff00112233445566778899" {
		t.Errorf("SessionSticky.Salt=%q", c.SessionSticky.Salt)
	}
	want := []string{"10.0.0.1", "172.17.0.0/16"}
	if len(c.TrustedProxies) != len(want) {
		t.Fatalf("TrustedProxies=%#v, want %#v (blank entries must be dropped)", c.TrustedProxies, want)
	}
	for i := range want {
		if c.TrustedProxies[i] != want[i] {
			t.Errorf("TrustedProxies[%d]=%q, want %q", i, c.TrustedProxies[i], want[i])
		}
	}
}

// TestDefaultTrustedProxiesIsEmpty: trusting nobody must be the default, or a
// spoofed X-Forwarded-For would defeat the console rate limiter out of the box.
func TestDefaultTrustedProxiesIsEmpty(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 0 {
		t.Errorf("default TrustedProxies=%#v, want empty", c.TrustedProxies)
	}
}

func TestPostgresConfig(t *testing.T) {
	t.Setenv("WB2A_POSTGRES_DSN", "postgres://user:pass@localhost/db")
	t.Setenv("WB2A_POSTGRES_MAX_OPEN_CONNS", "24")
	t.Setenv("WB2A_POSTGRES_MAX_IDLE_CONNS", "12")
	t.Setenv("WB2A_POSTGRES_CONN_MAX_LIFETIME", "1h")
	t.Setenv("WB2A_POSTGRES_CONN_MAX_IDLE_TIME", "2m")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Postgres.DSN == "" || c.Postgres.MaxOpenConns != 24 || c.Postgres.MaxIdleConns != 12 || c.PostgresMaxLifetime.Hours() != 1 || c.PostgresMaxIdleTime.Minutes() != 2 {
		t.Fatalf("postgres config=%+v durations=%v/%v", c.Postgres, c.PostgresMaxLifetime, c.PostgresMaxIdleTime)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHardCreditKeyIgnored(t *testing.T) {
	// 退役的 hard_credit 键作为 JSON 未知字段被自然忽略，不报错。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("hard_credit must be ignored (not validated): %v", err)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v want 30s", c.SoftRateDur)
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlight != 3 {
		t.Errorf("max_in_flight=%d want 3", c.Pool.MaxInFlight)
	}
	if c.Pool.BreakerThreshold != 3 {
		t.Errorf("breaker_threshold=%d want 3", c.Pool.BreakerThreshold)
	}
	if c.BreakerCooldownDur.Minutes() != 30 {
		t.Errorf("breaker_cooldown=%v want 30m", c.BreakerCooldownDur)
	}
	if c.BreakerCooldownMaxD.Hours() != 6 {
		t.Errorf("breaker_cooldown_max=%v want 6h", c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.5 || c.Pool.IdleWeightMax != 5.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if !c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want true")
	}
	if c.SessionTTL.Minutes() != 30 || c.SessionGCInterval.Minutes() != 5 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		t.Errorf("upstash default should be empty: %+v", c.Upstash)
	}
	if c.SMSProxyCooldownDur.Minutes() != 30 {
		t.Errorf("sms.proxy.cooldown=%v want 30m", c.SMSProxyCooldownDur)
	}
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"upstash":{"url":"https://foo.upstash.io","token":"tok"},
		"pool":{
			"max_in_flight":5,
			"breaker_threshold":4,
			"breaker_cooldown":"10m",
			"breaker_cooldown_max":"2h",
			"idle_weight_per_hour":0.7,
			"idle_weight_max":8.0
		},
		"session_sticky":{"enabled":false,"ttl":"1h","gc_interval":"2m"}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstash.URL != "https://foo.upstash.io" || c.Upstash.Token != "tok" {
		t.Errorf("upstash=%+v", c.Upstash)
	}
	if c.Pool.MaxInFlight != 5 || c.Pool.BreakerThreshold != 4 {
		t.Errorf("pool=%+v", c.Pool)
	}
	if c.BreakerCooldownDur.Minutes() != 10 || c.BreakerCooldownMaxD.Hours() != 2 {
		t.Errorf("breaker durations=%v/%v", c.BreakerCooldownDur, c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.7 || c.Pool.IdleWeightMax != 8.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want false from file")
	}
	if c.SessionTTL.Hours() != 1 || c.SessionGCInterval.Minutes() != 2 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

func TestBadSessionTTL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

// TestAutoEnrollLedgerPath 号码账本必须与 state.json 同目录：部署时那个目录
// 已挂载为卷，所以容器重建后账本还在，才能补释放遗留的号码。
func TestAutoEnrollLedgerPath(t *testing.T) {
	got := autoEnrollLedgerPath(&Config{StateFile: "./data/state.json"})
	want := filepath.Join("data", "autoenroll-held.json")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// 绝对路径同样只取目录。
	got = autoEnrollLedgerPath(&Config{StateFile: "/app/data/state.json"})
	if want := filepath.Join("/app/data", "autoenroll-held.json"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// 没配 state_file = 关闭持久化，而不是写到进程工作目录（那里没有卷）。
	if got := autoEnrollLedgerPath(&Config{}); got != "" {
		t.Fatalf("got %q want empty when state_file is unset", got)
	}
}

// pprof 默认关闭：生产默认不该多开一个无鉴权端口。
func TestPprofDisabledByDefault(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Pprof.Enabled {
		t.Error("pprof.enabled want false by default")
	}
	if c.PprofAddr != "" {
		t.Errorf("PprofAddr=%q want empty when disabled", c.PprofAddr)
	}
}

// 显式开启后：默认地址是 127.0.0.1:6060，env/文件可覆盖地址。
func TestPprofEnabledUsesLoopbackDefaultAndOverrides(t *testing.T) {
	t.Setenv("WB2A_PPROF_ENABLED", "true")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.PprofAddr != "127.0.0.1:6060" {
		t.Fatalf("PprofAddr=%q want 127.0.0.1:6060", c.PprofAddr)
	}

	t.Setenv("WB2A_PPROF_ADDR", "127.0.0.1:7777")
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.PprofAddr != "127.0.0.1:7777" {
		t.Fatalf("PprofAddr=%q want 127.0.0.1:7777", c.PprofAddr)
	}
}

// pprof 地址必须是显式 host:port 且绑定具体接口：无鉴权端点绑 0.0.0.0 等于
// 把堆数据（含内存里的密钥）开放给任何能连到该端口的人。":6060" 与空 host 同罪。
// addr="" 是合法的（= 用默认 127.0.0.1:6060），单独测；其余非法。
func TestPprofRejectsWildcardOrMissingHost(t *testing.T) {
	for _, addr := range []string{":6060", "0.0.0.0:6060", "[::]:6060", "6060", "   "} {
		c := Default()
		c.Pprof.Enabled = true
		c.Pprof.Addr = addr
		if err := c.normalize(); err == nil {
			t.Errorf("addr=%q want error (wildcard/missing host rejected)", addr)
		}
	}
	// 合法：具体回环/内网地址；空串回落默认地址。
	for _, addr := range []string{"127.0.0.1:6060", "10.0.0.5:6060", "[::1]:6060", ""} {
		c := Default()
		c.Pprof.Enabled = true
		c.Pprof.Addr = addr
		if err := c.normalize(); err != nil {
			t.Errorf("addr=%q unexpected error: %v", addr, err)
		}
		if c.PprofAddr == "" {
			t.Errorf("addr=%q PprofAddr empty after normalize", addr)
		}
	}
}
