package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrSessionInvalid = errors.New("session invalid")
	ErrSessionExpired = errors.New("session expired")
)

type Session struct {
	UserID    uuid.UUID `json:"uid"`
	ExpiresAt time.Time `json:"exp"`
}

type SessionSigner struct {
	key []byte
}

func NewSessionSigner(key []byte) *SessionSigner {
	if len(key) < 32 {
		panic("session signer key must be at least 32 bytes")
	}
	return &SessionSigner{key: key}
}

func (s *SessionSigner) Encode(sess Session) (string, error) {
	payload, err := json.Marshal(sess)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := s.sign(body)
	return body + "." + mac, nil
}

func (s *SessionSigner) Decode(token string) (Session, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return Session{}, ErrSessionInvalid
	}
	wantMac := s.sign(parts[0])
	if !hmac.Equal([]byte(wantMac), []byte(parts[1])) {
		return Session{}, ErrSessionInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Session{}, ErrSessionInvalid
	}
	var sess Session
	if err := json.Unmarshal(raw, &sess); err != nil {
		return Session{}, ErrSessionInvalid
	}
	if time.Now().After(sess.ExpiresAt) {
		return Session{}, ErrSessionExpired
	}
	return sess, nil
}

func (s *SessionSigner) sign(body string) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
