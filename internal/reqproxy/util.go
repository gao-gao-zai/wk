package reqproxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// randHex n 字节随机 hex。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
