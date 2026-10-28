package main

import (
	"path/filepath"
	"testing"
)

func TestHaozhumaWebTokenStartupRestore(t *testing.T) {
	t.Setenv("HAOZHUMA_WEB_TOKEN", "fixture-environment")
	cfg := Default()
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	c, err := loadHaozhumaWebClient(cfg, "fixture-key")
	if err != nil || !c.Info().Has || c.Info().Persisted {
		t.Fatal("initial environment fallback not available")
	}
	if err := c.SaveSession("fixture-saved"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAOZHUMA_WEB_TOKEN", "")
	restarted, err := loadHaozhumaWebClient(cfg, "fixture-key")
	if err != nil || !restarted.Info().Has || !restarted.Info().Persisted {
		t.Fatal("startup did not restore the saved Web token")
	}
	if err := restarted.SaveSession(""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAOZHUMA_WEB_TOKEN", "fixture-environment")
	cleared, err := loadHaozhumaWebClient(cfg, "fixture-key")
	if err != nil || cleared.Info().Has {
		t.Fatal("startup restored a previously cleared token")
	}
}
