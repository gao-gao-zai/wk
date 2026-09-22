package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// probeConfigWritable reports whether the config file can be rewritten by the
// console's save path (writeFileAtomicWith in internal/server).
//
// Why probe instead of trusting file mode: the deploy-time failure mode is not
// visible in os.Stat. A single-file Docker bind mount rejects rename (device or
// resource busy) but still accepts an in-place write, and a `:ro` mount rejects
// both. The first is a healthy deployment, the second silently breaks every
// console save until someone clicks "save" and reads the error. Replicating the
// exact write path (temp file + rename, fall back to in-place) turns that
// silent breakage into a startup log line.
//
// The probe is best-effort and non-destructive:
//   - it writes the current content back, byte for byte (no semantic change);
//   - any failure returns an error explaining the likely cause and fix.
//
// Called from main after Load succeeded; a missing file is NOT an error here
// (pure env-var deployments never create one) — Load already handles that case.
func probeConfigWritable(cfgPath string) error {
	if cfgPath == "" {
		return nil
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // env-only deployment; nothing to probe
		}
		return fmt.Errorf("read config for writable probe: %w", err)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		return fmt.Errorf("stat config for writable probe: %w", err)
	}
	mode := info.Mode().Perm()

	dir := filepath.Dir(cfgPath)
	tmp, err := os.CreateTemp(dir, ".wb2api-config-probe-*.tmp")
	if err != nil {
		// Directory not writable (e.g. read-only rootfs): in-place write may
		// still work, so continue to that path instead of failing here.
		tmp = nil
	}
	if tmp != nil {
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)
		if _, err := tmp.Write(raw); err != nil {
			tmp.Close()
			// Fall through to in-place probe.
		} else if err := tmp.Close(); err != nil {
			// Fall through to in-place probe.
		} else if err := os.Chmod(tmpPath, mode); err == nil {
			if err := os.Rename(tmpPath, cfgPath); err == nil {
				return nil // atomic replace works; fully writable
			}
		}
	}

	// In-place probe: same flags as writeFileInPlace in internal/server.
	f, err := os.OpenFile(cfgPath, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("config file is not writable: %w "+
			"(console saves via 管理设置 will fail; check the bind mount is not :ro and the file owner matches the container user)", err)
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("config file is not writable: %w "+
			"(console saves via 管理设置 will fail; check the bind mount is not :ro and the file owner matches the container user)", err)
	}
	// rename rejected (typical single-file bind mount) but in-place works:
	// the exact state the console's fallback path was designed for.
	return nil
}
