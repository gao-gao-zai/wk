// Package collection owns durable, bounded read jobs. Mutating growth actions
// and urgent chat authentication recovery deliberately have separate executors.
package collection

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

var Resources = []string{"balance", "usage", "credential", "models", "tasks"}

type Settings struct {
	Concurrency       int            `json:"concurrency"`
	RequestsPerSecond int            `json:"requests_per_second"`
	Automatic         bool           `json:"automatic"`
	Intervals         map[string]int `json:"intervals"` // seconds; also defines freshness
}

func DefaultSettings() Settings {
	return Settings{4, 4, true, map[string]int{"balance": 900, "usage": 300, "credential": 300, "models": 3600, "tasks": 1800}}
}
func (s Settings) Validate() error {
	if s.Concurrency < 1 || s.Concurrency > 16 || s.RequestsPerSecond < 1 || s.RequestsPerSecond > 20 {
		return fmt.Errorf("并发范围 1–16，请求速率范围 1–20/秒")
	}
	if len(s.Intervals) != len(Resources) {
		return fmt.Errorf("必须提供五种资源的周期")
	}
	for _, r := range Resources {
		if s.Intervals[r] < 60 || s.Intervals[r] > 86400 {
			return fmt.Errorf("%s 周期范围 60–86400 秒", r)
		}
	}
	return nil
}

// Store atomically commits changed records. Values never contain credentials.
type Store interface {
	CollectionLoad() (map[string][]byte, error)
	CollectionSave(map[string][]byte, []string) error
}
type Target struct {
	UID      string `json:"uid"`
	Resource string `json:"resource"`
}

func (t Target) key() string { return t.UID + "/" + t.Resource }

type Request struct {
	UIDs            []string `json:"uids"`
	Resources       []string `json:"resources"`
	IncludeDisabled bool     `json:"include_disabled"`
	Source          string   `json:"-"`
	Targets         []Target `json:"-"` // failed-only retry
}
type Item struct {
	Target
	Status      string `json:"status"`
	Attempts    int    `json:"attempts"`
	Error       string `json:"error,omitempty"`
	ErrorKind   string `json:"error_kind,omitempty"`
	StartedAt   int64  `json:"started_at,omitempty"`
	FinishedAt  int64  `json:"finished_at,omitempty"`
	NextRetryAt int64  `json:"next_retry_at,omitempty"`
}
type Job struct {
	ID              string  `json:"id"`
	Source          string  `json:"source"`
	Status          string  `json:"status"`
	CreatedAt       int64   `json:"created_at"`
	FinishedAt      int64   `json:"finished_at,omitempty"`
	IncludeDisabled bool    `json:"include_disabled"`
	Total           int     `json:"total"`
	Done            int     `json:"done"`
	Running         int     `json:"running"`
	Success         int     `json:"success"`
	Failed          int     `json:"failed"`
	Skipped         int     `json:"skipped"`
	Cancelled       int     `json:"cancelled"`
	Items           []*Item `json:"items,omitempty"`
}
type Snapshot struct {
	Target
	State       string          `json:"state"`
	LastAttempt int64           `json:"last_attempt,omitempty"`
	LastSuccess int64           `json:"last_success,omitempty"`
	NextRetryAt int64           `json:"next_retry_at,omitempty"`
	Error       string          `json:"error,omitempty"`
	ErrorKind   string          `json:"error_kind,omitempty"`
	Stale       bool            `json:"stale"`
	Invalidated bool            `json:"invalidated,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
}
type Failure struct {
	Kind, Message string
	Retryable     bool
	After         time.Duration
	Skip          bool
}

func (e *Failure) Error() string { return e.Message }

type Runner func(context.Context, Target, Snapshot) (json.RawMessage, error)
type Options struct {
	Store Store
	Run   Runner
	// Eligible is rechecked immediately before dispatch, including disabled state.
	Eligible func(uid string, includeDisabled bool) bool
	UIDs     func() []string
}
