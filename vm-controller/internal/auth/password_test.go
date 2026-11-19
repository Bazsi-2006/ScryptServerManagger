package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected encoding %q", h)
	}
	ctx := context.Background()
	if ok, err := VerifyPassword(ctx, "correct horse battery staple", h); !ok || err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if ok, _ := VerifyPassword(ctx, "correct horse battery stapl", h); ok {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery staple")
	if h == h2 {
		t.Fatal("salt is not random")
	}
}

func TestMalformedHashes(t *testing.T) {
	good, _ := hashWith(context.Background(), "pw", Params{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1})
	parts := strings.Split(good, "$")
	bad := []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=2$" + parts[4] + "$" + parts[5],
		"$argon2id$v=16$m=65536,t=3,p=2$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=99999999,t=3,p=2$" + parts[4] + "$" + parts[5], // absurd memory
		"$argon2id$v=19$m=65536,t=0,p=2$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=65536,t=3,p=2,x=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=65536,t=3,p=2$!!$" + parts[5],
		"$argon2id$v=19$m=65536,t=3,p=2$" + parts[4] + "$short",
	}
	for _, h := range bad {
		if ok, err := VerifyPassword(context.Background(), "pw", h); ok || err == nil {
			t.Errorf("hash %q: ok=%v err=%v", h, ok, err)
		}
	}
	if ok, err := VerifyPassword(context.Background(), "pw", good); !ok || err != nil {
		t.Fatalf("low-cost hash should still verify: %v", err)
	}
}

func TestVerifyRespectsContextWhenBusy(t *testing.T) {
	for i := 0; i < cap(slots); i++ {
		slots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(slots); i++ {
			<-slots
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	h := "$argon2id$v=19$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0$c2FsdHNhbHRzYWx0c2FsdHNhbHQ"
	if _, err := VerifyPassword(ctx, "pw", h); err != ErrBusy {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

func TestPasswordPolicy(t *testing.T) {
	for _, pw := range []string{"short", "elevenchars", " leading-space-pw", "trailing-space-pw ", "has\ttab-inside-it", strings.Repeat("x", MaxPasswordLen+1), "\xff\xfe invalid utf8 xx"} {
		if ValidateNewPassword(pw) == nil {
			t.Errorf("%q should be rejected", pw)
		}
	}
	for _, pw := range []string{"twelve chars", "correct horse battery staple", "ÁrvíztűrőTükör"} {
		if err := ValidateNewPassword(pw); err != nil {
			t.Errorf("%q rejected: %v", pw, err)
		}
	}
}

func TestTokens(t *testing.T) {
	a, err := NewToken()
	if err != nil || !WellFormedToken(a) {
		t.Fatalf("token %q %v", a, err)
	}
	b, _ := NewToken()
	if a == b {
		t.Fatal("tokens repeat")
	}
	if WellFormedToken(a[:42]) || WellFormedToken(a[:42]+"!") || WellFormedToken("") {
		t.Fatal("malformed tokens accepted")
	}
	if HashToken(a) == HashToken(b) || len(HashToken(a)) != 64 {
		t.Fatal("bad token hash")
	}
	if !EqualTokens(a, a) || EqualTokens(a, b) || EqualTokens(a, "") {
		t.Fatal("EqualTokens wrong")
	}
}

func BenchmarkHashPassword(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := HashPassword("benchmark password"); err != nil {
			b.Fatal(err)
		}
	}
}
