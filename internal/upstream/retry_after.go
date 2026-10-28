package upstream

import (
	"net/http"
	"strconv"
	"time"
)

func retryAfter(raw string) time.Duration {
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil && time.Until(t) > 0 {
		return time.Until(t)
	}
	return 0
}
