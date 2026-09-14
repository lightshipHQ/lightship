package store

import (
	"regexp"
	"strings"
	"testing"
)

// The generated password is read off one terminal and typed into another, so its shape is part of
// the contract: fixed grouping, and no character a person can misread for another.
func TestGenerateBootstrapPassword(t *testing.T) {
	shape := regexp.MustCompile(`^[a-z2-9]{4}(-[a-z2-9]{4}){5}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if !shape.MatchString(pw) {
			t.Fatalf("password %q does not match six dash-separated groups of four", pw)
		}
		if strings.ContainsAny(pw, "01ilo") {
			t.Fatalf("password %q contains an ambiguous character", pw)
		}
		for _, c := range pw {
			if c != '-' && !strings.ContainsRune(bootstrapAlphabet, c) {
				t.Fatalf("password %q contains %q, outside the alphabet", pw, c)
			}
		}
		if seen[pw] {
			t.Fatalf("password %q generated twice in 200 draws — the source is not random", pw)
		}
		seen[pw] = true
	}
}
