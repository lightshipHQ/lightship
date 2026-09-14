package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lightshipHQ/lightship/internal/auth"
)

func TestUpdateUserPasswordResetAndRemoval(t *testing.T) {
	st, _ := modelTestStores(t)
	ctx := context.Background()
	initialHash, err := auth.Hash("initial-test-password")
	if err != nil {
		t.Fatal(err)
	}
	resetHash, err := auth.Hash("reset-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(ctx, "owner", initialHash, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	creds, err := st.Credentials(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"first-test-session", "second-test-session"} {
		if err := st.CreateSession(ctx, creds.ID, token, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateKey(ctx, "automation", "test-api-key", "owner", nil); err != nil {
		t.Fatal(err)
	}
	if revoked, err := st.UpdateUser(ctx, "owner", &resetHash, nil); err != nil || revoked != 2 {
		t.Fatalf("reset password: revoked=%d err=%v", revoked, err)
	}
	for _, token := range []string{"first-test-session", "second-test-session"} {
		if _, err := st.Lookup(ctx, token); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("old session survived reset: %v", err)
		}
	}
	creds, err = st.Credentials(ctx, "owner")
	if err != nil || creds.PasswordHash != resetHash {
		t.Fatalf("replacement password was not stored: %v", err)
	}
	keySession, err := st.LookupKey(ctx, "test-api-key")
	if err != nil || !keySession.MustChangePassword {
		t.Fatalf("reset must preserve key but require enrollment: %v", err)
	}
	if _, err := st.SetOwnPassword(ctx, creds.ID, initialHash, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, creds.ID, "post-enrollment-test-session", time.Hour); err != nil {
		t.Fatal(err)
	}
	emptyHash := ""
	if revoked, err := st.UpdateUser(ctx, "owner", &emptyHash, nil); err != nil || revoked != 1 {
		t.Fatalf("remove password: revoked=%d err=%v", revoked, err)
	}
	if _, err := st.Credentials(ctx, "owner"); !errors.Is(err, ErrNoPassword) {
		t.Fatalf("password removal did not store NULL: %v", err)
	}
	keySession, err = st.LookupKey(ctx, "test-api-key")
	if err != nil || keySession.MustChangePassword {
		t.Fatalf("removal must preserve key without re-arming enrollment: %v", err)
	}
}
