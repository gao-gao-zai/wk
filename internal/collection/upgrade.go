package collection

// FlushChecked rewrites the final durable view after all accepted work has
// completed. A failed item/job snapshot write must prevent release handoff.
func (m *Manager) FlushChecked() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := map[string][]byte{"settings": encode(m.settings)}
	for id, j := range m.jobs {
		rows["job/"+id] = jobJSON(j)
		for _, item := range j.Items {
			rows[itemKey(id, item.Target)] = encode(item)
		}
	}
	for key, s := range m.snapshots {
		rows["snapshot/"+key] = encode(s)
	}
	return m.save(rows, nil)
}
