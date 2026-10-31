package server

import (
	"context"
	"errors"
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/lifecycle"
	"workbuddy2api/internal/upstream"
)

const (
	authRecoveryCooldown = time.Minute
	authRecoveryTimeout  = 15 * time.Second
)

// Quarantine before releasing the failed attempt. The client can immediately
// rotate while one bounded refresh uses the account's existing proxy route.
func (h *Handler) recoverUnauthorized(a *auth.Auth) {
	id, started := h.cfg.Pool.BeginAuthRecovery(a, authRecoveryCooldown)
	if !started {
		return
	}
	lifecycle.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), authRecoveryTimeout)
		defer cancel()
		err := h.cfg.Upstream.RefreshTokenContext(ctx, a)
		reason := ""
		var ue *upstream.Error
		if errors.As(err, &ue) {
			switch ue.Kind {
			case upstream.ErrAccountDisabled:
				reason = "upstream account disabled (token refresh)"
			case upstream.ErrSessionDead:
				reason = "12153 session dead (token refresh)"
			}
		}
		if !h.cfg.Pool.FinishAuthRecovery(a, id, reason) {
			return
		}
		if err != nil {
			// Never print the upstream body: diagnostics may contain credentials.
			log.Printf("auth recovery uid=%s: refresh failed; disabled=%t", a.UID, reason != "")
			return
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("auth recovery uid=%s: save auth failed: %v", a.UID, err)
			return
		}
		log.Printf("auth recovery uid=%s: refreshed; quarantine retained until retry window", a.UID)
	})
}
