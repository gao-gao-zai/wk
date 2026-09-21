package reqproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchSubscriptionBase64(t *testing.T) {
	body := "trojan://pw@hk.example.com:443#香港A\nvless://u@jp.example.com:443#日本B\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "v2rayN/6.60" {
			t.Errorf("缺少 v2rayN UA: %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("subscription-userinfo", "upload=100; download=900; total=1024; expire=1735689600")
		_, _ = w.Write([]byte(b64(body)))
	}))
	defer srv.Close()

	res, err := FetchSubscription(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("节点数: %d", len(res.Nodes))
	}
	if res.Userinfo == nil {
		t.Fatal("userinfo 未解析")
	}
	if res.Userinfo.UploadBytes != 100 || res.Userinfo.TotalBytes != 1024 {
		t.Errorf("userinfo: %+v", res.Userinfo)
	}
	if res.Userinfo.ExpireAt == nil || !res.Userinfo.ExpireAt.Equal(time.Unix(1735689600, 0)) {
		t.Errorf("expire: %+v", res.Userinfo.ExpireAt)
	}
}

func TestFetchSubscriptionClashYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("proxies:\n  - name: x\n    type: ss\n"))
	}))
	defer srv.Close()
	_, err := FetchSubscription(srv.URL)
	if err != ErrClashYAML {
		t.Fatalf("期望 ErrClashYAML, got %v", err)
	}
}

func TestFetchSubscriptionUnsupportedWarn(t *testing.T) {
	body := "hysteria2://abc@x.com:443#hy2\ntrojan://pw@a.com:443#ok\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	res, err := FetchSubscription(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 1 {
		t.Fatalf("节点数: %d", len(res.Nodes))
	}
	if len(res.Warnings) != 1 {
		t.Errorf("warnings: %v", res.Warnings)
	}
}

func TestParseUserinfoHeader(t *testing.T) {
	if u := parseUserinfoHeader(""); u != nil {
		t.Errorf("空头应返回 nil")
	}
	if u := parseUserinfoHeader("upload=1; download=2; total=10"); u == nil || u.TotalBytes != 10 {
		t.Errorf("userinfo: %+v", u)
	}
	if u := parseUserinfoHeader("garbage"); u != nil {
		t.Errorf("垃圾头应返回 nil: %+v", u)
	}
}
