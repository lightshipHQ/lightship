package store

import (
	"context"
	"errors"
	"testing"

	"github.com/lightshipHQ/lightship/internal/auth"
)

func TestPasswordWritesRejectInvalidHashesBeforeDatabaseAccess(t *testing.T) {
	st := &Store{}
	ctx := context.Background()
	invalid := "$argon2id$v=19$m=65536,t=3,p=0$invalid$"
	tests := map[string]func() error{
		"bootstrap":         func() error { _, err := st.EnsureAdmin(ctx, invalid); return err },
		"create":            func() error { return st.CreateUser(ctx, "alice", invalid, nil, nil, true) },
		"reset":             func() error { _, err := st.UpdateUser(ctx, "alice", &invalid, nil); return err },
		"self change":       func() error { _, err := st.SetOwnPassword(ctx, "alice-id", invalid, ""); return err },
		"empty self change": func() error { _, err := st.SetOwnPassword(ctx, "alice-id", "", ""); return err },
	}
	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, auth.ErrInvalidHash) {
				t.Fatalf("error = %v, want ErrInvalidHash", err)
			}
		})
	}
}
