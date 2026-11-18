// Package auth contains the credential primitives: Argon2id password hashing,
// HS256 access tokens and opaque random tokens.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters.
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

const (
	saltLen = 16
	keyLen  = 32
)

// hashSlots bounds the number of concurrent Argon2 computations so a burst of
// logins cannot exhaust memory (each computation uses MemoryKiB).
var hashSlots = make(chan struct{}, 4)

func derive(password string, salt []byte, p Params, n uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, n)
}

// HashPassword returns a PHC-formatted Argon2id hash:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
func HashPassword(password string, p Params) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := derive(password, salt, p, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

var errBadHash = errors.New("malformed password hash")

func parseHash(encoded string) (Params, []byte, []byte, error) {
	var p Params
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errBadHash
	}
	var par uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Iterations, &par); err != nil {
		return p, nil, nil, errBadHash
	}
	// Refuse absurd stored parameters instead of allocating for them.
	if par < 1 || par > 255 || p.Iterations < 1 || p.Iterations > 100 || p.MemoryKiB < 8 || p.MemoryKiB > 4*1024*1024 {
		return p, nil, nil, errBadHash
	}
	p.Parallelism = uint8(par)
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return p, nil, nil, errBadHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 {
		return p, nil, nil, errBadHash
	}
	return p, salt, key, nil
}

// VerifyPassword checks password against an encoded hash in constant time.
// needsRehash is true when the hash was produced with parameters other than
// want, so the caller can transparently upgrade it.
func VerifyPassword(password, encoded string, want Params) (ok bool, needsRehash bool) {
	p, salt, key, err := parseHash(encoded)
	if err != nil {
		return false, false
	}
	got := derive(password, salt, p, uint32(len(key)))
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false
	}
	return true, p != want || len(key) != keyLen
}

// NewToken returns a URL-safe random token with nBytes of entropy.
func NewToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// MustToken is NewToken for callers that cannot proceed without randomness.
func MustToken(nBytes int) string {
	t, err := NewToken(nBytes)
	if err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return t
}

// HashToken returns the hex SHA-256 of a token. High-entropy random tokens do
// not need a slow hash; storing only the digest keeps database leaks from
// yielding usable tokens.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
