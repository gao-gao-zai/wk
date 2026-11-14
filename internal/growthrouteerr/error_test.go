package growthrouteerr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReasonAllowlistAndRedaction(t *testing.T) {
	const secret = "https://uid:private-password@private-host/?token=private-token"
	for code := range messages {
		err := fmt.Errorf("%s: %w", secret, New(code))
		if got := Reason(err); got != code {
			t.Errorf("got %s want %s", got, code)
		}
		public := Public("growth task proxy route unavailable", err).Error()
		if public != "growth task proxy route unavailable [reason="+string(code)+"]" || strings.Contains(public, secret) {
			t.Errorf("unsafe public error for code %s", code)
		}
	}
	for _, tc := range []struct {
		err  error
		code Code
	}{
		{errors.New(secret), Unknown},
		{fmt.Errorf("%s: %w", secret, context.Canceled), Canceled},
		{fmt.Errorf("%s: %w", secret, context.DeadlineExceeded), Deadline},
		{New(Code(secret)), Unknown},
	} {
		if Reason(tc.err) != tc.code {
			t.Errorf("wrong reason, want %s", tc.code)
		}
		if strings.Contains(Public("route", tc.err).Error(), secret) {
			t.Fatal("secret retained")
		}
	}
}
