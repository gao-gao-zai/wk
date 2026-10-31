package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"time"
)

func sessionDigest(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

type unlockWire struct {
	WindowStart  time.Time
	Failures     int
	BlockedUntil time.Time
}
type handoffWire struct {
	Version         int
	Sessions        map[string]time.Time
	Attempts        map[string]unlockWire
	Failures        int
	Window, Blocked time.Time
	Bindings        json.RawMessage
}

func (h *Handler) handoffCipher() (cipher.AEAD, error) {
	secret := h.cfg.APIKey
	if secret == "" {
		secret = h.cfg.FrontendPassword
	}
	if secret == "" {
		return nil, errors.New("handoff requires stable administrator credential")
	}
	k := sha256.Sum256([]byte("wb2api/upgrade/v1\x00" + secret))
	b, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// SaveHandoff must be called after both foreground and background are drained.
func (h *Handler) SaveHandoff(path string, bindings []byte) error {
	s := handoffWire{Version: 1, Bindings: bindings, Attempts: map[string]unlockWire{}}
	h.sessionsMu.Lock()
	s.Sessions = make(map[string]time.Time, len(h.sessions))
	for k, v := range h.sessions {
		if time.Now().Before(v) {
			s.Sessions[k] = v
		}
	}
	h.sessionsMu.Unlock()
	h.unlockRateMu.Lock()
	for k, v := range h.unlockAttempts {
		s.Attempts[k] = unlockWire{v.windowStart, v.failures, v.blockedUntil}
	}
	s.Failures = h.unlockGlobalFailures
	s.Window = h.unlockGlobalWindowStart
	s.Blocked = h.unlockGlobalBlockedUntil
	h.unlockRateMu.Unlock()
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	a, err := h.handoffCipher()
	if err != nil {
		return err
	}
	n := make([]byte, a.NonceSize())
	if _, err = rand.Read(n); err != nil {
		return err
	}
	return writeFileAtomic(path, a.Seal(n, n, raw, []byte("wb2api-handoff-v1")), 0600)
}
func (h *Handler) LoadHandoff(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := h.handoffCipher()
	if err != nil {
		return nil, err
	}
	if len(raw) < a.NonceSize() {
		return nil, errors.New("invalid handoff")
	}
	plain, err := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte("wb2api-handoff-v1"))
	if err != nil {
		return nil, errors.New("handoff authentication failed")
	}
	var s handoffWire
	if err = json.Unmarshal(plain, &s); err != nil {
		return nil, err
	}
	if s.Version != 1 {
		return nil, errors.New("unsupported handoff version")
	}
	h.sessionsMu.Lock()
	for k, v := range s.Sessions {
		if time.Now().Before(v) {
			h.sessions[k] = v
		}
	}
	h.sessionsMu.Unlock()
	h.unlockRateMu.Lock()
	for k, v := range s.Attempts {
		h.unlockAttempts[k] = unlockAttempt{v.WindowStart, v.Failures, v.BlockedUntil}
	}
	h.unlockGlobalFailures = s.Failures
	h.unlockGlobalWindowStart = s.Window
	h.unlockGlobalBlockedUntil = s.Blocked
	h.unlockRateMu.Unlock()
	return s.Bindings, nil
}
