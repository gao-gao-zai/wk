package pool

import "fmt"

// FlushChecked is the handoff commit barrier; normal periodic flush retains
// its historical retry behavior. Call only after all producers are drained.
func (p *Pool) FlushChecked() error {
	p.Flush()
	p.persistMu.Lock()
	defer p.persistMu.Unlock()
	if p.persistFails != 0 {
		return fmt.Errorf("pool state persistence failed")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.dirtySet) != 0 {
		return fmt.Errorf("pool has uncommitted account changes")
	}
	return nil
}
