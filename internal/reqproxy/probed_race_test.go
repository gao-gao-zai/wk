package reqproxy

import (
	"sync"
	"testing"
	"time"
)

// TestProbedFlagSurvivesConcurrentSnapshot 复现：100 节点并发测速时
// MarkProbed 设置的 Probed 标志是否会被并发操作冲掉。
func TestProbedFlagRace(t *testing.T) {
	p := newTestPool(t)
	// 建 100 个节点
	specs := make([]NodeSpec, 100)
	for i := range specs {
		specs[i] = NodeSpec{
			ID: nodeIDN(i), Name: nodeIDN(i), Protocol: "http",
			Source: "manual",
			Spec:   map[string]any{"host": "127.0.0.1", "port": 8080},
		}
	}
	p.SetNodes(specs)

	// 并发：模拟 RunOnce 的 100 goroutine probe + MarkProbed
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(i%10) * time.Millisecond) // 错开一点模拟网络
			p.MarkProbed(nodeIDN(i), int64(100+i), nil)
		}(i)
	}
	// 同时并发跑 Candidates / Views / NodesSnapshot（模拟 after 回调和 API 读）
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				p.Candidates("cn")
				p.Views()
				p.NodesSnapshot()
			}
		}
	}()
	wg.Wait()
	close(stop)

	// 断言：所有节点都 Probed
	unprobed := []string{}
	for _, n := range p.NodesSnapshot() {
		if !n.Probed {
			unprobed = append(unprobed, n.ID)
		}
	}
	if len(unprobed) > 0 {
		t.Fatalf("%d 个节点 Probed 丢失: %v", len(unprobed), unprobed[:min(5, len(unprobed))])
	}
}

func nodeIDN(i int) string {
	return "node_test_" + string(rune('a'+i/26)) + string(rune('a'+i%26))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
