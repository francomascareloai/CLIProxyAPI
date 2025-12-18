package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestRoundRobinSelectorPick_ModelCooldownWinsOverDisabledAuths(t *testing.T) {
	now := time.Now()

	authCooldown := &Auth{
		ID: "a",
		ModelStates: map[string]*ModelState{
			"m": {
				Unavailable:    true,
				NextRetryAfter: now.Add(2 * time.Second),
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: now.Add(2 * time.Second),
				},
			},
		},
	}

	authDisabled := &Auth{ID: "b", Disabled: true}

	s := &RoundRobinSelector{}
	_, err := s.Pick(context.Background(), "p", "m", cliproxyexecutor.Options{}, []*Auth{authCooldown, authDisabled})
	if err == nil {
		t.Fatalf("expected error")
	}
	se, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected StatusCode() on error, got %T", err)
	}
	if got := se.StatusCode(); got != 429 {
		t.Fatalf("expected 429, got %d (%T: %v)", got, err, err)
	}
}

func TestAuthErrorStatusCodeDefaults(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{code: "provider_not_found", want: 400},
		{code: "auth_not_found", want: 503},
		{code: "auth_unavailable", want: 503},
	}

	for _, tc := range cases {
		err := &Error{Code: tc.code, Message: "x"}
		if got := err.StatusCode(); got != tc.want {
			t.Fatalf("code=%s: expected %d, got %d", tc.code, tc.want, got)
		}
	}
}
