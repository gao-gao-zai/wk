package reqproxy

import "fmt"

func (m *Manager) Ready() bool {
	select {
	case <-m.initialized:
	default:
		return false
	}
	if len(m.pool.NodesSnapshot()) == 0 {
		return true
	}
	return len(m.pool.Candidates("cn"))+len(m.pool.Candidates("global")) > 0
}

func (m *Manager) FlushChecked() error {
	m.store.Flush()
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	if m.store.dirty {
		return fmt.Errorf("proxy state persistence failed")
	}
	return nil
}
