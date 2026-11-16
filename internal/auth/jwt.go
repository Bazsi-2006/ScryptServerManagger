package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Claims are the contents of an access token. Authorization data (admin
// flag, permissions) is deliberately not embedded: it is looked up on every
// request so revocations take effect immediately.
type Claims struct {
	UserID    int64  `json:"sub"`
	SessionID string `json:"sid"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Issuer    string `json:"iss"`
}

const (
	jwtIssuer = "server-manager"
	// The header is fixed; tokens with any other header (including other
	// "alg" values such as "none") are rejected outright.
	jwtHeader = `{"alg":"HS256","typ":"JWT"}`
)

var (
	ErrTokenInvalid = errors.New("invalid access token")
	ErrTokenExpired = errors.New("access token expired")

	b64          = base64.RawURLEncoding
	encodedHead  = b64.EncodeToString([]byte(jwtHeader))
	maxTokenSize = 2048
)

// Signer issues and verifies HS256 access tokens.
type Signer struct {
	key []byte
}

func NewSigner(key []byte) *Signer { return &Signer{key: append([]byte(nil), key...)} }

func (s *Signer) mac(msg string) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// Issue creates a signed token valid for ttl.
func (s *Signer) Issue(userID int64, sessionID string, now time.Time, ttl time.Duration) (string, time.Time) {
	exp := now.Add(ttl)
	payload, _ := json.Marshal(Claims{UserID: userID, SessionID: sessionID, IssuedAt: now.Unix(), ExpiresAt: exp.Unix(), Issuer: jwtIssuer})
	msg := encodedHead + "." + b64.EncodeToString(payload)
	return msg + "." + b64.EncodeToString(s.mac(msg)), exp
}

// Verify checks the signature and expiry and returns the claims.
func (s *Signer) Verify(token string, now time.Time) (*Claims, error) {
	if len(token) > maxTokenSize {
		return nil, ErrTokenInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != encodedHead {
		return nil, ErrTokenInvalid
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, s.mac(parts[0]+"."+parts[1])) {
		return nil, ErrTokenInvalid
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrTokenInvalid
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil || c.Issuer != jwtIssuer || c.UserID <= 0 || c.SessionID == "" {
		return nil, ErrTokenInvalid
	}
	if now.Unix() >= c.ExpiresAt {
		return nil, ErrTokenExpired
	}
	return &c, nil
}
