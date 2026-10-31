package session

import (
	"encoding/json"
	"fmt"
	"time"
)

type bindingWire struct {
	UID        string
	LastActive time.Time
	Version    uint64
}

// Export contains only salted key digests, never raw conversation identifiers.
func (r *Router) Export() ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]bindingWire{}
	for k, v := range r.entries {
		if !expired(v, time.Now(), r.cfg.TTL) {
			out[k] = bindingWire{v.uid, v.lastActive, v.version}
		}
	}
	return json.Marshal(out)
}
func (r *Router) Import(raw []byte) error {
	var in map[string]bindingWire
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	if len(in) > r.cfg.MaxEntries {
		return fmt.Errorf("handoff binding limit exceeded")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range in {
		e := entry{uid: v.UID, lastActive: v.LastActive, version: v.Version}
		if len(k) != 64 || expired(e, time.Now(), r.cfg.TTL) {
			continue
		}
		r.entries[k] = e
		if v.Version > r.version {
			r.version = v.Version
		}
	}
	return nil
}
