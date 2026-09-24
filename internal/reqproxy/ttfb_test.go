package reqproxy

import (
	"strings"
	"testing"
	"time"
)

func TestFirstFrameReadsContentFrame(t *testing.T) {
	body := strings.NewReader("" +
		": keep-alive\n" +
		"\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n" +
		"\n" +
		"data: [DONE]\n")
	start := time.Now().Add(-1500 * time.Millisecond)
	got, err := firstFrame(body, start)
	if err != nil {
		t.Fatal(err)
	}
	if got < 1000 || got > 10_000 {
		t.Fatalf("ttfb=%d ms，应落在约 1500ms 附近", got)
	}
}

func TestFirstFrameEmptyStream(t *testing.T) {
	_, err := firstFrame(strings.NewReader("data: [DONE]\n"), time.Now())
	if err == nil {
		t.Fatal("没有内容帧应报错")
	}
}

// fakeTTFBSource 固定返回若干账号。
type fakeTTFBSource struct{ n int }

func (f fakeTTFBSource) AccountsForGroup(group string) []TTFBAccount {
	if group == "空" {
		return nil
	}
	out := make([]TTFBAccount, f.n)
	return out
}

func TestTTFBRunRejectsEmptyGroup(t *testing.T) {
	p := newTestPool(t)
	prober := NewTTFBProber(p, nil)
	prober.SetAccountSource(fakeTTFBSource{n: 1})

	if _, _, err := prober.Run("空", "", 1, time.Second); err == nil {
		t.Fatal("空分组应拒绝启动")
	}
}

func TestTTFBRunRejectsConcurrent(t *testing.T) {
	p := newTestPool(t)
	prober := NewTTFBProber(p, nil)
	prober.SetAccountSource(fakeTTFBSource{n: 1})
	prober.mu.Lock()
	prober.running = true
	prober.mu.Unlock()

	if _, _, err := prober.Run("", "", 1, time.Second); err == nil {
		t.Fatal("已有一轮在跑时应拒绝")
	}
}
