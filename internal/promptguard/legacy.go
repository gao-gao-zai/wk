package promptguard

import "errors"

// LegacyConfig is the effective startup configuration (file plus environment).
// It is consulted only once. Subsequent restarts use the encrypted rule store.
type LegacyConfig struct {
	Codex bool
	DSH   bool
}

func legacyRules(c LegacyConfig) []Rule {
	return []Rule{
		{ID: "legacy-codex-identity-v1", Name: "Codex 身份说明", Source: "legacy_codex", Enabled: c.Codex,
			Match:       "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful.",
			Replacement: "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open-source project led by OpenAI. You are expected to be precise, safe, and helpful."},
		{ID: "legacy-dsh-gui-v1", Name: "DSH Web GUI 说明", Source: "legacy_dsh", Enabled: c.DSH,
			Match:       "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI.",
			Replacement: "You are communicating with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI."},
		{ID: "legacy-dsh-paths-v1", Name: "DSH 路径引用说明", Source: "legacy_dsh", Enabled: c.DSH,
			Match:       "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host.",
			Replacement: "Paths explicitly referenced by the user are prefixed with @. Resolve relative paths from the workspace root; absolute paths refer to files or directories on the host."},
		{ID: "legacy-dsh-identity-v1", Name: "DSH 身份说明", Source: "legacy_dsh", Enabled: c.DSH, RequireEndBoundary: true,
			Match: "You are an AI agent powered by DeepSeek Harness", Replacement: "You are an AI agent running on DeepSeek Harness"},
	}
}

func (m *Manager) LegacyMigrated() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Config.LegacyMigrationVersion >= 1
}

// MigrateLegacy appends presets after custom rules, preserving the old pipeline
// order. Marker and rules commit together; a failed write rolls everything back.
// Deletions/edits/disabled states after migration are never repopulated on restart.
func (m *Manager) MigrateLegacy(c LegacyConfig) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false, errors.New("prompt guard closed")
	}
	if m.state.Config.LegacyMigrationVersion >= 1 {
		return false, nil
	}
	old := copyJSON(m.state)
	ids := map[string]bool{}
	for _, r := range m.state.Config.Rules {
		ids[r.ID] = true
	}
	for _, preset := range legacyRules(c) {
		duplicate := false
		for _, r := range m.state.Config.Rules {
			if r.Match == preset.Match && r.Replacement == preset.Replacement && r.Enabled == preset.Enabled && r.RequireEndBoundary == preset.RequireEndBoundary {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		for ids[preset.ID] {
			preset.ID += "-migrated"
		}
		ids[preset.ID] = true
		m.state.Config.Rules = append(m.state.Config.Rules, preset)
	}
	if len(m.state.Config.Rules) > 256 {
		m.state = old
		return false, errors.New("not enough room to migrate legacy prompt rules")
	}
	m.state.Config.LegacyMigrationVersion = 1
	m.state.Config.Revision++
	for _, item := range m.state.Cases {
		item.Resolved = false
	}
	if err := m.saveLocked(); err != nil {
		m.state = old
		return false, err
	}
	return true, nil
}
