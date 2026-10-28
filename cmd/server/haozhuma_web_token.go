package main

import (
	"os"
	"path/filepath"
	"strings"

	"workbuddy2api/internal/haozhumah5"
)

func loadHaozhumaWebClient(cfg *Config, secret string) (*haozhumah5.Client, error) {
	path := ""
	if strings.TrimSpace(cfg.StateFile) != "" {
		path = filepath.Join(filepath.Dir(cfg.StateFile), "haozhuma-web-token.enc")
	}
	return haozhumah5.NewPersistent(strings.TrimSpace(os.Getenv("HAOZHUMA_WEB_TOKEN")), path, secret)
}
