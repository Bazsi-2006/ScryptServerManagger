package auth

import (
	"strings"
	"testing"
	"time"
)

var fast = Params{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1}

func TestPasswordHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse", fast)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("unexpected format: %s", h)
	}
	if strings.Contains(h, "correct horse") {
		t.Fatal("hash contains the plaintext")
	}
	ok, rehash := VerifyPassword("correct horse", h, fast)
	if !ok || rehash {
		t.Fatalf("ok=%v rehash=%v", ok, rehash)
	}
	if ok, _ := VerifyPassword("wrong", h, fast); ok {
		t.Fatal("wrong password accepted")
	}
	if ok, _ := VerifyPassword("", h, fast); ok {
		t.Fatal("empty password accepted")
	}
}

func TestPasswordSaltsDiffer(t *testing.T) {
	a, _ := HashPassword("same", fast)
	b, _ := HashPassword("same", fast)
	if a == b {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}
}

func TestRehashWhenParamsChange(t *testing.T) {
	h, _ := HashPassword("pw-pw-pw", fast)
	stronger := Params{MemoryKiB: 16 * 1024, Iterations: 2, Parallelism: 1}
	ok, rehash := VerifyPassword("pw-pw-pw", h, stronger)
	if !ok || !rehash {
		t.Fatalf("ok=%v rehash=%v, want true/true", ok, rehash)
	}
}

func TestMalformedHashesRejected(t *testing.T) {
	for _, h := range []string{
		"", "plaintext", "$argon2i$v=19$m=8192,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=18$m=8192,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=999999999,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=8192,t=1,p=1$!!!$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=8192,t=1,p=1$c2FsdHNhbHQ$",
	} {
		if ok, _ := VerifyPassword("x", h, fast); ok {
			t.Errorf("malformed hash accepted: %q", h)
		}
	}
}

func TestJWTRoundTrip(t *testing.T) {
	s := NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Now()
	tok, exp := s.Issue(42, "sess1", now, time.Minute)
	if !exp.After(now) {
		t.Fatal("expiry not in the future")
	}
	c, err := s.Verify(tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.UserID != 42 || c.SessionID != "sess1" {
		t.Fatalf("claims: %+v", c)
	}
	if _, err := s.Verify(tok, now.Add(2*time.Minute)); err != ErrTokenExpired {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
}

func TestJWTRejectsForgery(t *testing.T) {
	s := NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	other := NewSigner([]byte("ffffffffffffffffffffffffffffffff"))
	now := time.Now()
	tok, _ := s.Issue(1, "s", now, time.Minute)
	if _, err := other.Verify(tok, now); err != ErrTokenInvalid {
		t.Fatal("token signed with another key accepted")
	}
	parts := strings.Split(tok, ".")
	// alg=none downgrade.
	none := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + parts[1] + "."
	if _, err := s.Verify(none, now); err != ErrTokenInvalid {
		t.Fatal("alg=none token accepted")
	}
	// Tampered payload with the original signature.
	forged := parts[0] + "." + b64.EncodeToString([]byte(`{"sub":2,"sid":"s","iat":1,"exp":99999999999,"iss":"server-manager"}`)) + "." + parts[2]
	if _, err := s.Verify(forged, now); err != ErrTokenInvalid {
		t.Fatal("tampered payload accepted")
	}
	for _, bad := range []string{"", "a.b", "a.b.c.d", strings.Repeat("x", 5000)} {
		if _, err := s.Verify(bad, now); err == nil {
			t.Errorf("garbage accepted: %q", bad[:min(len(bad), 10)])
		}
	}
}

func TestTokensAreRandomAndHashed(t *testing.T) {
	a, b := MustToken(32), MustToken(32)
	if a == b || len(a) < 40 {
		t.Fatal("tokens must be long and unique")
	}
	if HashToken(a) == a || HashToken(a) != HashToken(a) || HashToken(a) == HashToken(b) {
		t.Fatal("HashToken must be a deterministic digest")
	}
}
