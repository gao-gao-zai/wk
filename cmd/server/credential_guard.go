package main

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// decodeSessionSalt 解析配置里的 hex 盐。
// 非法输入返回错误而不是静默回退到空盐：悄悄降级会让"以为配了盐"的部署
// 实际使用可预计算的键，是最难排查的一类问题。
func decodeSessionSalt(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("session_sticky.salt must be hex: %w", err)
	}
	if len(b) < 16 {
		return nil, fmt.Errorf("session_sticky.salt must be at least 16 bytes (%d given)", len(b))
	}
	return b, nil
}

// placeholderSecrets are the credential values shipped in config.example.json
// and baked into the container image. They are published in the repository, so
// accepting one is equivalent to having no credential at all — an attacker can
// read the value and then use every route.
var placeholderSecrets = map[string]bool{
	"your-api-key-here":    true,
	"change-this-password": true,
	"changeme":             true,
	"password":             true,
	"secret":               true,
}

// isPublicPlaceholder reports whether v is a non-empty, publicly known example
// value. An empty value is NOT a placeholder: deliberately leaving one
// credential unset is legitimate as long as the other one is real.
func isPublicPlaceholder(v string) bool {
	return placeholderSecrets[strings.TrimSpace(v)]
}

// requireRealCredential refuses to start unless a usable credential is
// configured.
//
// The previous guard only fired when the config file was MISSING, so the most
// dangerous combination — file present, contents still the public example
// values — was allowed straight through. Dockerfile used to copy
// config.example.json to /app/config.json, which produced exactly that state.
//
// Rules:
//   - both credentials empty            → refuse (nothing would authenticate)
//   - either credential is a known
//     public placeholder (non-empty)    → refuse (a published value is not a secret)
//   - one real credential, other empty  → allow
func requireRealCredential(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("refusing to start: nil config")
	}
	apiKey := strings.TrimSpace(cfg.APIKey)
	password := strings.TrimSpace(cfg.FrontendPassword)

	switch {
	case isPublicPlaceholder(apiKey):
		return fmt.Errorf("refusing to start: api_key is a public example value; " +
			"set a real one via api_key or WB2A_API_KEY")
	case isPublicPlaceholder(password):
		return fmt.Errorf("refusing to start: frontend_password is a public example value; " +
			"set a real one via frontend_password or WB2A_FRONTEND_PASSWORD")
	case apiKey == "" && password == "":
		return fmt.Errorf("refusing to start: neither api_key nor frontend_password is set " +
			"(WB2A_API_KEY / WB2A_FRONTEND_PASSWORD); refusing to start without authentication")
	}
	return nil
}
