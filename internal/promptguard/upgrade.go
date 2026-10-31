package promptguard

// FlushChecked is called only after the upgrade barrier has drained probes.
func (m *Manager) FlushChecked() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked()
}
