package reqproxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// newTestManager 纯逻辑 Manager（kernel=nil；后台循环由 Close 收尾）。
// state 落临时文件，模拟真实启动路径。
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(Config{StateFile: filepath.Join(t.TempDir(), "state.json")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// TestRefreshFailKeepsManualNodes 回归：订阅刷新失败（网络挂/DNS 失败）时，
// manual 节点必须仍留在池里。旧行为：FetchSubscription 失败提前 return，
// 池不重建——manual 节点不进池，WebUI 节点列表里"被自动删掉"。
func TestRefreshFailKeepsManualNodes(t *testing.T) {
	m := newTestManager(t)

	// 导入 manual 节点（走 ImportManualNodes → rebuildPool 正常路径）
	if _, err := m.ImportManualNodes("1.2.3.4:8080:user:pass"); err != nil {
		t.Fatal(err)
	}
	if got := len(m.NodesSnapshot()); got != 1 {
		t.Fatalf("导入后池应 1 个节点, got %d", got)
	}

	// 添加一个必然失败的订阅（端口 1 不可达）
	m.Update(func(st *State) {
		st.Subscriptions = append(st.Subscriptions, Subscription{
			ID: "sub_dead", Name: "挂掉的订阅", URL: "http://127.0.0.1:1/sub",
			Interval: "1h",
		})
	})

	// 刷新失败：manual 节点必须保留在池里
	if err := m.RefreshSubscription("sub_dead"); err == nil {
		t.Fatal("订阅刷新应失败")
	}
	nodes := m.NodesSnapshot()
	if len(nodes) != 1 || nodes[0].Source != "manual" {
		t.Fatalf("刷新失败后 manual 节点应保留, got %+v", nodes)
	}
}

// TestRefreshSuccessKeepsManual 订阅刷新成功：订阅节点进池，manual 节点保留。
func TestRefreshSuccessKeepsManual(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.ImportManualNodes("1.2.3.4:8080:user:pass"); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("trojan://pw@10.0.0.1:443#测试A\n"))
	}))
	defer srv.Close()

	m.Update(func(st *State) {
		st.Subscriptions = append(st.Subscriptions, Subscription{
			ID: "sub_ok", Name: "好订阅", URL: srv.URL, Interval: "1h",
		})
	})
	if err := m.RefreshSubscription("sub_ok"); err != nil {
		t.Fatal(err)
	}
	var manual, sub int
	for _, n := range m.NodesSnapshot() {
		if n.Source == "manual" {
			manual++
		} else {
			sub++
		}
	}
	if manual != 1 || sub != 1 {
		t.Fatalf("manual=%d sub=%d, want 1/1", manual, sub)
	}
}

// TestManualNodeNotAutoDeleted manual 节点不会因健康检查失败被移出 state：
// unhealthy 只影响分配（Candidates 过滤），不删 ManualNodes 记录。
func TestManualNodeNotAutoDeleted(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.ImportManualNodes("1.2.3.4:8080:user:pass"); err != nil {
		t.Fatal(err)
	}
	nodeID := m.NodesSnapshot()[0].ID
	// 连续失败 → unhealthy（MarkProbed 第 3 次起置 Unhealthy）
	for i := 0; i < 6; i++ {
		m.pool.MarkProbed(nodeID, -1, errFailed)
	}
	var cnt int
	m.View(func(st *State) { cnt = len(st.ManualNodes) })
	if cnt != 1 {
		t.Fatalf("manual 节点不应被健康检查删除, got %d", cnt)
	}
	// unhealthy 只是不参与分配，节点还在池里
	if got := len(m.NodesSnapshot()); got != 1 {
		t.Fatalf("unhealthy 节点应留在池视图, got %d", got)
	}
}

// TestNoSubscriptionManualNodesSurviveRestart 无订阅时 manual 节点也必须
// 进池（启动路径 rebuildPool）。旧行为：池重建只发生在订阅刷新成功后，
// "无订阅"或"订阅全挂"时 manual 节点永远不进池。
func TestNoSubscriptionManualNodesSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	// 第一次运行：导入 manual 节点
	m1, err := NewManager(Config{StateFile: fp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m1.ImportManualNodes("1.2.3.4:8080:user:pass"); err != nil {
		t.Fatal(err)
	}
	m1.Close()

	// 第二次运行（模拟重启，无任何订阅）
	m2, err := NewManager(Config{StateFile: fp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	// 启动 goroutine 是异步的，等它跑完 rebuildPool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(m2.NodesSnapshot()) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	nodes := m2.NodesSnapshot()
	if len(nodes) != 1 || nodes[0].Source != "manual" {
		t.Fatalf("重启后 manual 节点应自动进池, got %+v", nodes)
	}
}

var errFailed = &probeError{}

type probeError struct{}

func (e *probeError) Error() string { return "probe failed" }
