package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGrowthConcurrencyStartupConfig(t *testing.T) {
	t.Setenv("WB2A_GROWTH_CONCURRENCY", "")
	if c, err := Load(""); err != nil || c.GrowthConcurrency != 16 {
		t.Fatalf("default: %v %v", c, err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	for _, tc := range []struct {
		raw   string
		want  int
		valid bool
	}{{`{}`, 16, true}, {`{"growth_concurrency":48}`, 48, true}, {`{"growth_concurrency":0}`, 0, false}, {`{"growth_concurrency":129}`, 0, false}} {
		_ = os.WriteFile(path, []byte(tc.raw), 0600)
		c, err := Load(path)
		if tc.valid {
			if err != nil || c.GrowthConcurrency != tc.want {
				t.Fatalf("config=%s c=%v err=%v", tc.raw, c, err)
			}
		} else if err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	_ = os.WriteFile(path, []byte(`{"growth_concurrency":48}`), 0600)
	t.Setenv("WB2A_GROWTH_CONCURRENCY", "24")
	if c, err := Load(path); err != nil || c.GrowthConcurrency != 24 {
		t.Fatalf("env override: %v %v", c, err)
	}
	t.Setenv("WB2A_GROWTH_CONCURRENCY", "invalid")
	if _, err := Load(path); err == nil {
		t.Fatal("malformed env accepted")
	}
}
