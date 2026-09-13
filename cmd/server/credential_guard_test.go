package main

import "testing"

func TestRequireRealCredentialRejectsPublicPlaceholders(t *testing.T) {
	cases := []struct {
		name     string
		apiKey   string
		password string
		wantErr  bool
	}{
		// The exact values shipped in config.example.json / the image.
		{"example api key", "your-api-key-here", "change-this-password", true},
		{"example api key alone", "your-api-key-here", "", true},
		{"example password alone", "", "change-this-password", true},
		{"weak common value", "secret", "", true},
		{"nothing configured", "", "", true},
		{"whitespace only", "   ", "  ", true},
		// Legitimate configurations.
		{"real api key", "s3cr3t-8f2a1c9e", "", false},
		{"real password", "", "c0rrect-horse-battery", false},
		{"both real", "k-9d2f", "p-4a7b", false},
		// Empty is allowed when the other credential is real: leaving one
		// mechanism unused is a valid deployment choice.
		{"key real password empty", "k-9d2f", "", false},
		{"key empty password real", "", "p-4a7b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireRealCredential(&Config{APIKey: tc.apiKey, FrontendPassword: tc.password})
			if tc.wantErr && err == nil {
				t.Errorf("api_key=%q password=%q: expected refusal, got nil", tc.apiKey, tc.password)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("api_key=%q password=%q: unexpected refusal: %v", tc.apiKey, tc.password, err)
			}
		})
	}
}

func TestRequireRealCredentialRejectsNilConfig(t *testing.T) {
	if err := requireRealCredential(nil); err == nil {
		t.Error("nil config must be refused")
	}
}

// TestDefaultConfigShipsNoCredential documents that Default() alone cannot start
// the service: the guard makes an unconfigured deployment a hard failure instead
// of a silently unauthenticated one.
func TestDefaultConfigShipsNoCredential(t *testing.T) {
	cfg := Default()
	if cfg.APIKey != "" || cfg.FrontendPassword != "" {
		t.Fatalf("Default() must not ship a credential: api_key=%q password=%q", cfg.APIKey, cfg.FrontendPassword)
	}
	if err := requireRealCredential(cfg); err == nil {
		t.Error("Default() config must be refused by the startup guard")
	}
}

// TestDefaultListenIsLoopback pins the binding default. A bare ":7863" binds
// every interface, which is how an authentication bug turns into a
// remotely-reachable one.
func TestDefaultListenIsLoopback(t *testing.T) {
	if got := Default().Listen; got != "127.0.0.1:7863" {
		t.Errorf("Default().Listen = %q, want loopback", got)
	}
}

// TestExampleConfigIsRejected guards the shipped example: it must not be usable
// as a live config without editing, because Dockerfile used to install it as one.
func TestExampleConfigIsRejected(t *testing.T) {
	cfg, err := Load("../../config.example.json")
	if err != nil {
		t.Fatalf("example config must stay parseable: %v", err)
	}
	if err := requireRealCredential(cfg); err == nil {
		t.Error("config.example.json must not satisfy the startup guard; it ships public placeholders")
	}
}
