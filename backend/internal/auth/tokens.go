package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	ErrInvalidToken = errors.New("token is invalid or expired")
	ErrTokenReused  = errors.New("refresh token was already used")
)

// Claims is what travels inside the access token. Keep it minimal: a JWT is
// signed, not encrypted — anyone holding it can read every field. Never put an
// email, a name, or anything you would not print in a log.
type Claims struct {
	UserID uuid.UUID `json:"uid"`
	Role   string    `json:"role"`
	jwt.RegisteredClaims
}

type TokenIssuer struct {
	accessSecret  []byte
	refreshSecret []byte
	accessTTL     time.Duration
	refreshTTL    time.Duration
}

func NewTokenIssuer(accessSecret, refreshSecret string, accessTTL, refreshTTL time.Duration) *TokenIssuer {
	return &TokenIssuer{
		accessSecret:  []byte(accessSecret),
		refreshSecret: []byte(refreshSecret),
		accessTTL:     accessTTL,
		refreshTTL:    refreshTTL,
	}
}

// RefreshTTL is how long a refresh token stays valid. The service needs it to
// compute expires_at, and the handler needs it for the cookie's Max-Age, so the
// two can never drift apart into a cookie that outlives its row.
func (t *TokenIssuer) RefreshTTL() time.Duration { return t.refreshTTL }

// AccessTTL is surfaced so the login response can tell the client when to
// refresh, instead of the client hardcoding a number that is wrong after the
// first time anyone tunes ACCESS_TOKEN_TTL.
func (t *TokenIssuer) AccessTTL() time.Duration { return t.accessTTL }

func (t *TokenIssuer) NewAccessToken(userID uuid.UUID, role string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID: userID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    "creatorflow",
			Audience:  jwt.ClaimStrings{"creatorflow-api"},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(t.accessTTL)),
			NotBefore: jwt.NewNumericDate(now),
			ID:        uuid.NewString(),
		},
	}

	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.accessSecret)
}

// ParseAccessToken verifies signature, expiry, issuer and audience.
func (t *TokenIssuer) ParseAccessToken(raw string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(raw, &Claims{},
		func(tok *jwt.Token) (any, error) {
			// Pinning the algorithm is not optional. Without it an attacker can
			// present a token signed with "none", or an HMAC token verified
			// against a public key, and the library will happily accept it.
			if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", tok.Header["alg"])
			}
			return t.accessSecret, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer("creatorflow"),
		jwt.WithAudience("creatorflow-api"),
	)
	if err != nil {
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	return claims, nil
}

// NewRefreshToken returns the raw token (given to the client, never stored) and
// its SHA-256 hash (stored, never leaves the server). A leaked database must
// not hand an attacker working sessions.
func NewRefreshToken() (raw string, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}

	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashRefreshToken(raw), nil
}

func (t *TokenIssuer) ParseUserID(raw string) (uuid.UUID, string, error) {
	claims, err := t.ParseAccessToken(raw)
	if err != nil {
		return uuid.Nil, "", err
	}
	return claims.UserID, claims.Role, nil
}

// HashRefreshToken is plain SHA-256, deliberately. Unlike a password, this is
// 256 bits of cryptographic randomness — there is no dictionary to attack, so
// the slow-hash cost that protects passwords buys nothing here and would add
// latency to every refresh.
func HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
