// Package auth holds the credential primitives: Argon2id password hashing
// and random session tokens.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Params are Argon2id cost parameters.
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

// DefaultParams: 64 MiB, 3 passes, 2 lanes. This is above the OWASP minimum
// (19 MiB, 2 passes) and takes roughly 100-200 ms on a modern server core.
var DefaultParams = Params{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 2}

const (
	saltLen = 16
	keyLen  = 32

	// MinPasswordLen and MaxPasswordLen bound new passwords (in bytes for
	// the maximum, in characters for the minimum).
	MinPasswordLen = 12
	MaxPasswordLen = 1024
)

// ErrBusy is returned when too many hashes are being computed at once.
var ErrBusy = errors.New("auth: password hashing capacity exhausted")

// slots bounds concurrent Argon2 computations so a burst of login attempts
// cannot exhaust memory (each computation allocates MemoryKiB).
var slots = make(chan struct{}, 2)

func derive(ctx context.Context, password []byte, salt []byte, p Params, n uint32) ([]byte, error) {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ErrBusy
	}
	defer func() { <-slots }()
	return argon2.IDKey(password, salt, p.Iterations, p.MemoryKiB, p.Parallelism, n), nil
}

// HashPassword returns a PHC-formatted Argon2id hash:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
func HashPassword(password string) (string, error) {
	return hashWith(context.Background(), password, DefaultParams)
}

// HashPasswordWithParams hashes with explicit parameters. Verification reads
// the parameters from the hash, so this is mainly useful for fast tests.
func HashPasswordWithParams(password string, p Params) (string, error) {
	return hashWith(context.Background(), password, p)
}

func hashWith(ctx context.Context, password string, p Params) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := derive(ctx, []byte(password), salt, p, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// ErrMalformedHash means a stored hash could not be parsed.
var ErrMalformedHash = errors.New("auth: malformed password hash")

func parseHash(encoded string) (p Params, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return p, nil, nil, ErrMalformedHash
	}
	var m, t, par uint64
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return p, nil, nil, ErrMalformedHash
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return p, nil, nil, ErrMalformedHash
		}
		switch k {
		case "m":
			m = n
		case "t":
			t = n
		case "p":
			par = n
		default:
			return p, nil, nil, ErrMalformedHash
		}
	}
	// Refuse absurd parameters instead of allocating for them.
	if m < 8*1024 || m > 1024*1024 || t < 1 || t > 32 || par < 1 || par > 64 {
		return p, nil, nil, ErrMalformedHash
	}
	p = Params{MemoryKiB: uint32(m), Iterations: uint32(t), Parallelism: uint8(par)}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(salt) < 8 {
		return p, nil, nil, ErrMalformedHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(key) < 16 || len(key) > 64 {
		return p, nil, nil, ErrMalformedHash
	}
	return p, salt, key, nil
}

// VerifyPassword checks password against an encoded hash in constant time.
// ctx bounds how long the call waits for a free hashing slot.
func VerifyPassword(ctx context.Context, password, encoded string) (bool, error) {
	p, salt, key, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	got, err := derive(ctx, []byte(password), salt, p, uint32(len(key)))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// VerifyDummy spends the same effort as VerifyPassword without a real hash.
// It is called for unknown users so response timing does not reveal which
// usernames exist.
func VerifyDummy(ctx context.Context, password string) {
	dummyOnce.Do(func() {
		h, err := HashPassword("dummy password used for timing equalization")
		if err != nil {
			panic("auth: cannot compute dummy hash: " + err.Error())
		}
		dummyHash = h
	})
	_, _ = VerifyPassword(ctx, password, dummyHash)
}

// ValidateNewPassword enforces the policy for passwords set via the CLI.
func ValidateNewPassword(password string) error {
	if !utf8.ValidString(password) {
		return errors.New("password must be valid UTF-8")
	}
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(password) > MaxPasswordLen {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordLen)
	}
	if strings.IndexFunc(password, unicode.IsControl) >= 0 {
		return errors.New("password must not contain control characters")
	}
	if strings.TrimSpace(password) != password {
		return errors.New("password must not start or end with whitespace")
	}
	return nil
}

// NewToken returns a URL-safe random token with 256 bits of entropy.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// TokenLen is the length of tokens produced by NewToken.
const TokenLen = 43

// WellFormedToken reports whether s looks like a token from NewToken.
func WellFormedToken(s string) bool {
	if len(s) != TokenLen {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}

// HashToken returns the hex SHA-256 of a session token. Session tokens are
// random and long, so a fast hash suffices; storing only the digest means a
// copy of the database does not contain usable sessions.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// EqualTokens compares two tokens in constant time.
func EqualTokens(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
