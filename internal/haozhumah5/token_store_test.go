package haozhumah5

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestPersistentWebTokenRestartAndClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "web-token.enc")
	c, err := NewPersistent("fixture-environment", path, "fixture-key")
	if err != nil || !c.Info().Has || c.Info().Persisted {
		t.Fatal("environment fallback should remain transient")
	}
	if err := c.SaveSession("fixture-web-token"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || bytes.Contains(data, []byte("fixture-web-token")) {
		t.Fatal("credential must be encrypted at rest")
	}
	stat, _ := os.Stat(path)
	if runtime.GOOS != "windows" && stat.Mode().Perm() != 0600 {
		t.Fatalf("unexpected credential file permissions: %v", stat.Mode())
	}
	restored, err := NewPersistent("different-environment", path, "fixture-key")
	if err != nil || restored.session != "fixture-web-token" || !restored.Info().Persisted {
		t.Fatal("saved credential did not survive restart")
	}
	if err := restored.SaveSession(""); err != nil {
		t.Fatal(err)
	}
	cleared, err := NewPersistent("fixture-environment", path, "fixture-key")
	if err != nil || cleared.Info().Has || cleared.Info().Persisted {
		t.Fatal("clear must survive restart and override the environment fallback")
	}
}

func TestPersistentWebTokenFailurePreservesOldValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-token.enc")
	c, err := NewPersistent("", path, "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SaveSession("fixture-old-token"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	c.tokenStore.path = filepath.Join(path, "not-a-directory")
	for _, token := range []string{"fixture-new-token", ""} {
		if c.SaveSession(token) == nil {
			t.Fatal("writing below a regular file must fail")
		}
		if c.session != "fixture-old-token" || !c.Info().Persisted || c.Info().StorageError == "" {
			t.Fatal("failed save must preserve the active credential and report failure")
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed save changed the previous encrypted store")
	}
}

func TestPersistentWebTokenCorruptStoreRecovery(t *testing.T) {
	for _, kind := range []string{"tampered", "wrong-key", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "web-token.enc")
			c, _ := NewPersistent("", path, "fixture-key")
			if err := c.SaveSession("fixture-old-token"); err != nil {
				t.Fatal(err)
			}
			secret := "fixture-key"
			data, _ := os.ReadFile(path)
			switch kind {
			case "tampered":
				data[len(data)-1] ^= 1
			case "truncated":
				data = data[:4]
			case "wrong-key":
				secret = "different-key"
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			recovered, err := NewPersistent("fixture-environment", path, secret)
			if err == nil || recovered.Info().Has || recovered.Info().StorageError == "" {
				t.Fatal("bad storage must not silently fall back to another credential")
			}
			if err := recovered.SaveSession("fixture-new-token"); err != nil {
				t.Fatal(err)
			}
			restored, err := NewPersistent("", path, secret)
			if err != nil || restored.session != "fixture-new-token" || restored.Info().StorageError != "" {
				t.Fatal("explicit save should recover the store")
			}
		})
	}
}

func TestPersistentWebTokenConcurrentSaveAndInfo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-token.enc")
	c, _ := NewPersistent("", path, "fixture-key")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if c.SaveSession("fixture-concurrent-token") != nil {
					t.Error("concurrent save failed")
				}
				_ = c.Info()
			}
		}()
	}
	wg.Wait()
	restored, err := NewPersistent("", path, "fixture-key")
	if err != nil || restored.session != c.session || !restored.Info().Persisted {
		t.Fatal("last committed credential must match the active value")
	}
}

func TestPersistentWebTokenWithoutStorage(t *testing.T) {
	c, err := NewPersistent("fixture-environment", "", "")
	if err != nil || c.Info().Storage != "memory" || !c.Info().Has {
		t.Fatal("memory-only operation must remain available")
	}
	c, err = NewPersistent("fixture-environment", filepath.Join(t.TempDir(), "token.enc"), "")
	if err == nil || c.Info().Storage != "memory" || !c.Info().Has || c.Info().StorageError == "" {
		t.Fatal("missing encryption key must be reported without claiming persistence")
	}
}
