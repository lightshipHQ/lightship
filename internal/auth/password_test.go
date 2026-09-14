package auth

import (
	"errors"
	"strings"
	"testing"
)

const validEncoding = "$argon2id$v=19$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestValidateHashCanonicalProfile(t *testing.T) {
	if len(validEncoding) != 97 {
		t.Fatalf("fixture length = %d, want 97", len(validEncoding))
	}
	if err := ValidateHash(validEncoding); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(validEncoding, "$")
	malformed := map[string]string{
		"empty":               "",
		"plaintext":           "password",
		"algorithm":           strings.Replace(validEncoding, "argon2id", "argon2i", 1),
		"version":             strings.Replace(validEncoding, "v=19", "v=18", 1),
		"version suffix":      strings.Replace(validEncoding, "v=19", "v=19x", 1),
		"leading text":        "x" + validEncoding,
		"trailing text":       validEncoding + "x",
		"extra field":         validEncoding + "$",
		"zero memory":         strings.Replace(validEncoding, "m=65536", "m=0", 1),
		"unsupported memory":  strings.Replace(validEncoding, "m=65536", "m=65537", 1),
		"huge memory":         strings.Replace(validEncoding, "m=65536", "m=4294967295", 1),
		"negative memory":     strings.Replace(validEncoding, "m=65536", "m=-1", 1),
		"zero iterations":     strings.Replace(validEncoding, "t=3", "t=0", 1),
		"large iterations":    strings.Replace(validEncoding, "t=3", "t=4294967295", 1),
		"zero lanes":          strings.Replace(validEncoding, "p=4", "p=0", 1),
		"unsupported lanes":   strings.Replace(validEncoding, "p=4", "p=8", 1),
		"overflow lanes":      strings.Replace(validEncoding, "p=4", "p=256", 1),
		"parameter suffix":    strings.Replace(validEncoding, "p=4", "p=4junk", 1),
		"parameter order":     strings.Replace(validEncoding, "m=65536,t=3,p=4", "t=3,m=65536,p=4", 1),
		"duplicate parameter": strings.Replace(validEncoding, "p=4", "p=4,p=4", 1),
		"leading zero":        strings.Replace(validEncoding, "t=3", "t=03", 1),
		"empty salt":          hashPrefix + "$" + parts[5],
		"short salt":          hashPrefix + parts[4][:21] + "$" + parts[5],
		"long salt":           hashPrefix + parts[4] + "AA$" + parts[5],
		"empty digest":        hashPrefix + parts[4] + "$",
		"short digest":        validEncoding[:len(validEncoding)-1],
		"long digest":         validEncoding + "A",
		"padded salt":         hashPrefix + parts[4][:21] + "=$" + parts[5],
		"invalid salt":        hashPrefix + "!" + parts[4][1:] + "$" + parts[5],
		"invalid digest":      validEncoding[:len(validEncoding)-1] + "!",
		"noncanonical salt":   hashPrefix + parts[4][:21] + "B$" + parts[5],
		"noncanonical digest": validEncoding[:len(validEncoding)-1] + "B",
		"newline salt":        hashPrefix + parts[4][:21] + "\n$" + parts[5],
		"newline digest":      validEncoding[:len(validEncoding)-1] + "\n",
	}
	for name, encoded := range malformed {
		t.Run(name, func(t *testing.T) {
			if err := ValidateHash(encoded); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("ValidateHash error = %v, want ErrInvalidHash", err)
			}
			if err := Verify("password", encoded); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("Verify error = %v, want ErrInvalidHash", err)
			}
		})
	}
}

func TestInvalidHashRejectedWithoutAllocating(t *testing.T) {
	for _, encoded := range []string{
		strings.Repeat("A", 1<<20),
		strings.Replace(validEncoding, "m=65536", "m=65537", 1),
		hashPrefix + strings.Repeat("A", saltEncodedLen) + "$",
	} {
		allocations := testing.AllocsPerRun(100, func() {
			if err := Verify("password", encoded); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("Verify error = %v, want ErrInvalidHash", err)
			}
		})
		if allocations != 0 {
			t.Errorf("invalid hash allocated %v times, want zero", allocations)
		}
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	encoded, err := Hash("owner-chosen-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateHash(encoded); err != nil {
		t.Fatalf("generated hash rejected: %v", err)
	}
	if err := Verify("owner-chosen-password", encoded); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if err := Verify("different-password", encoded); !errors.Is(err, ErrMismatch) {
		t.Fatalf("wrong password error = %v, want ErrMismatch", err)
	}
}
