// Package growthlimit shares one growth admission budget with private routes.
package growthlimit

import (
	"fmt"
	"sync"
)

const Default = 16
const Maximum = 128

type Budget struct {
	mu       sync.Mutex
	limit    int
	admitted int
}

func Validate(n int) error {
	if n < 1 || n > Maximum {
		return fmt.Errorf("growth_concurrency must be between 1 and %d", Maximum)
	}
	return nil
}

func New(n int) *Budget {
	if Validate(n) != nil {
		n = Default
	}
	return &Budget{limit: n}
}

func (b *Budget) Current() int {
	if b == nil {
		return Default
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit
}

func (b *Budget) Set(n int) error {
	if err := Validate(n); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limit = n
	return nil
}

func (b *Budget) Enter() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.admitted >= b.limit {
		return false
	}
	b.admitted++
	return true
}

func (b *Budget) Leave() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.admitted--
}

// LeaseLimit never revokes admission: a shrink only prevents future Enter calls.
// Route acquisition does not Enter again (one permit per account, not per layer).
func (b *Budget) LeaseLimit() int {
	if b == nil {
		return Default
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(b.limit, b.admitted)
}
