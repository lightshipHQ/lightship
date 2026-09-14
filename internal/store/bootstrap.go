package store

// The generated credential. When LIGHTSHIP_ADMIN_PASSWORD_HASH is unset and no admin password is
// stored, EnsureAdmin (store.go) generates one here, stores only its argon2id hash, and hands the
// password back for the caller to print exactly once.

import (
	"context"
	"crypto/rand"
	"strings"
)

// bootstrapAlphabet is chosen for a password that is read off one screen and typed into another:
// lowercase and digits with the lookalikes removed (0/o, 1/l/i), grouped by dashes in the output.
// 31 symbols over 24 characters is ~119 bits of entropy — far beyond any guessing margin, and the
// stored form is an argon2id hash besides.
const (
	bootstrapAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	bootstrapGroups   = 6
	bootstrapGroupLen = 4
)

// GeneratePassword returns a password shaped like k7x2-9fqm-3wnd-p8vt-2c6h-a4rn. It is exported
// because the provisioning API generates a user's first password the same way the first boot
// generates the admin's: the two credentials are handed over identically — read once off a screen,
// typed once into another, and replaced at first login — so they should be the same string, drawn
// the same way, rather than two generators that could drift in alphabet or entropy.
func GeneratePassword() (string, error) {
	// Rejection sampling keeps the draw uniform: 248 is the largest multiple of 31 below 256, so a
	// byte at or above it would fold unevenly onto the alphabet and is redrawn instead.
	limit := byte(256 - 256%len(bootstrapAlphabet))
	chars := make([]byte, 0, bootstrapGroups*bootstrapGroupLen)
	buf := make([]byte, 64)
	for len(chars) < cap(chars) {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b >= limit || len(chars) == cap(chars) {
				continue
			}
			chars = append(chars, bootstrapAlphabet[int(b)%len(bootstrapAlphabet)])
		}
	}
	groups := make([]string, 0, bootstrapGroups)
	for i := 0; i < len(chars); i += bootstrapGroupLen {
		groups = append(groups, string(chars[i:i+bootstrapGroupLen]))
	}
	return strings.Join(groups, "-"), nil
}

// ConsumeBootstrapFirstLogin reports whether this login is the first on a generated credential,
// clearing the marker as it answers so that exactly one login can ever be it — concurrent logins
// race on the row update, and only one sees it flip. The generated password was printed to a log
// stream a collector may ship elsewhere, so the login handler records this one login as
// auth.bootstrap_first_login rather than auth.login: "did anyone but me use that line?" becomes a
// query instead of an inference.
func (s *Store) ConsumeBootstrapFirstLogin(ctx context.Context, userID string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`update app_user set bootstrap_login_pending = false
		  where id = $1 and bootstrap_login_pending`, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
