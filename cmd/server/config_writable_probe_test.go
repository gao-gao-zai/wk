package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeConfigWritableAtomicReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeConfigWritable(path); err != nil {
		t.Fatalf("regular writable file must pass: %v", err)
	}
	// 内容必须原样保留（探测是非破坏性的）。
	raw, _ := os.ReadFile(path)
	if string(raw) != `{"api_key":"k"}` {
		t.Fatalf("probe must not alter content, got %q", raw)
	}
	// 探测后目录里不能留下临时文件。mode 保真不做断言：探测刻意复刻
	// 生产写入路径（writeFileAtomicWith 的 temp+rename），Go 的 Chmod 在
	// Windows 上对非只读位是 no-op，跨平台断言 mode 会误报；Linux 生产
	// 环境下行为与控制台保存完全一致。
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("probe left %d entries, want 1 (the config itself)", len(entries))
	}
}

func TestProbeConfigWritableBindMountFallback(t *testing.T) {
	// 单文件 bind mount 形态：rename 被拒（device or resource busy），
	// 但 in-place 写可行——控制台回落路径正是为这个形态设计的。
	// Linux 上真正复现需要 mount --bind，CI 里不可行；该场景由
	// TestProbeConfigWritableInPlaceOnly 等价模拟（temp 创建失败 + 文件可写）。
	// 本用例验证正常文件的基础健康 + 探测非破坏性。
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("cfg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeConfigWritable(path); err != nil {
		t.Fatalf("writable file: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "cfg" {
		t.Fatalf("probe must not alter content, got %q", raw)
	}
}

func TestProbeConfigWritableInPlaceOnly(t *testing.T) {
	// 目录不可写（temp 创建失败）但文件本身可写：探测必须回落到 in-place
	// 并成功——这正是单文件 bind mount 的内核形态在用户态的等价模拟。
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("cfg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // 去掉目录写权限
		t.Skipf("cannot chmod dir: %v", err)
	}
	defer os.Chmod(dir, 0o700)
	err := probeConfigWritable(path)
	if err != nil {
		t.Fatalf("in-place writable file must pass, got: %v", err)
	}
}

func TestProbeConfigWritableReadOnlyFile(t *testing.T) {
	// :ro 挂载形态：temp 可建（目录可写）但 rename 被拒 + 文件本身只读。
	// 期望：返回带修复指引的错误（main 里以 [warning] 打日志）。
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("cfg"), 0o400); err != nil { // 只读文件
		t.Fatal(err)
	}
	// 探测以当前用户跑；root 下 0o400 仍可写，非 root 才能测出失败。
	if os.Getuid() == 0 {
		t.Skip("running as root: read-only mode does not block writes, cannot simulate :ro")
	}
	err := probeConfigWritable(path)
	if err == nil {
		t.Fatal("read-only file must be reported")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("want permission error, got: %v", err)
	}
}

func TestProbeConfigWritableMissingFile(t *testing.T) {
	// 纯 env 部署：文件不存在，探测跳过（不是错误）。
	if err := probeConfigWritable(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatalf("missing config must not error: %v", err)
	}
}

func TestProbeConfigWritableEmptyPath(t *testing.T) {
	if err := probeConfigWritable(""); err != nil {
		t.Fatalf("empty path must not error: %v", err)
	}
}
