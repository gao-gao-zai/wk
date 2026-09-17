package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSMSDebugWritesFullDetail 诊断日志必须记录完整手机号与短信原文
// （不脱敏），且只落本机文件、不进控制台回显的 logs 数组。
func TestSMSDebugWritesFullDetail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sms-debug.log")
	en := NewAutoEnroller(nil, nil, "52283", nil, nil, nil)
	en.SetSMSDebugPath(path)
	en.pollInterval = time.Millisecond

	en.smsDebug("[%s] 轮 1 收到短信 phone=%s code=%s sms=%q", "52283", "19812344941", "483912", "【腾讯科技】验证码483912，60秒内有效")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "19812344941") {
		t.Fatalf("full phone must be recorded, got: %s", text)
	}
	if !strings.Contains(text, "483912") {
		t.Fatalf("code must be recorded, got: %s", text)
	}
	if !strings.Contains(text, "验证码483912，60秒内有效") {
		t.Fatalf("full sms text must be recorded, got: %s", text)
	}
	// 不进控制台回显（那是脱敏面）。
	en.mu.Lock()
	logs := append([]string(nil), en.logs...)
	en.mu.Unlock()
	for _, l := range logs {
		if strings.Contains(l, "19812344941") {
			t.Fatalf("full phone leaked into console logs: %s", l)
		}
	}
	// 文件权限 0600（Windows 无 POSIX 权限位，跳过）。
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0600 {
			t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
		}
	}
}

// TestSMSDebugRotation 超过 1 MiB 上限时截掉前一半，且从整行边界开始。
func TestSMSDebugRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sms-debug.log")
	// 预填接近上限的内容（每行 100 字节 × 10400 行 ≈ 1.04 MB）。
	var sb strings.Builder
	line := strings.Repeat("x", 99) + "\n" // 100 字节
	for i := 0; i < 10400; i++ {
		sb.WriteString(line)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0600); err != nil {
		t.Fatal(err)
	}
	en := NewAutoEnroller(nil, nil, "52283", nil, nil, nil)
	en.SetSMSDebugPath(path)
	en.smsDebug("NEW-MARKER phone=%s", "19800000001")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxSMSDebugBytes+4096 {
		t.Fatalf("rotation failed: size=%d", len(raw))
	}
	if !strings.Contains(string(raw), "NEW-MARKER") {
		t.Fatal("new entry lost after rotation")
	}
	// 首行必须是完整行：要么以时间戳开头（新写入的），要么恰好 100
	// 字节（预填行长度，被切成两半的行长度不对）。
	first := strings.SplitN(string(raw), "\n", 2)[0]
	if len(first) != 100 && len(first) < 20 {
		t.Fatalf("rotation kept a partial line: %q (%d bytes)", first[:min(40, len(first))], len(first))
	}
	// 所有预填行都完整（都是 99 x + \n）。
	for _, l := range strings.Split(string(raw), "\n") {
		if l == "" || strings.Contains(l, "NEW-MARKER") || strings.Contains(l, "phone=") {
			continue
		}
		// 时间戳开头的行格式不同，跳过；纯 x 行必须完整。
		if strings.HasPrefix(l, "x") && len(l) != 99 {
			t.Fatalf("partial line after rotation: %q (%d bytes)", l[:min(40, len(l))], len(l))
		}
	}
}

// TestSMSDebugDisabledWithoutPath 未设置路径时是纯 no-op（不建文件）。
func TestSMSDebugDisabledWithoutPath(t *testing.T) {
	dir := t.TempDir()
	en := NewAutoEnroller(nil, nil, "52283", nil, nil, nil)
	en.smsDebug("phone=%s", "19800000001")
	if _, err := os.Stat(filepath.Join(dir, "sms-debug.log")); !os.IsNotExist(err) {
		t.Fatal("no file should be created when path is unset")
	}
}
