package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSessionRoundTrip(t *testing.T) {
	signer := NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	uid := uuid.New()

	encoded, err := signer.Encode(Session{UserID: uid, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	got, err := signer.Decode(encoded)
	require.NoError(t, err)
	require.Equal(t, uid, got.UserID)
}

func TestSessionRejectsTamperedSignature(t *testing.T) {
	signer := NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	encoded, _ := signer.Encode(Session{UserID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour)})
	tampered := encoded[:len(encoded)-1] + "X"

	_, err := signer.Decode(tampered)
	require.ErrorIs(t, err, ErrSessionInvalid)
}

func TestSessionRejectsExpired(t *testing.T) {
	signer := NewSessionSigner([]byte("test-secret-min-32-bytes-yes-yes-yes"))
	encoded, _ := signer.Encode(Session{UserID: uuid.New(), ExpiresAt: time.Now().Add(-time.Minute)})

	_, err := signer.Decode(encoded)
	require.ErrorIs(t, err, ErrSessionExpired)
}

func TestSessionRejectsForeignSecret(t *testing.T) {
	a := NewSessionSigner([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	b := NewSessionSigner([]byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))

	encoded, _ := a.Encode(Session{UserID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour)})
	_, err := b.Decode(encoded)
	require.ErrorIs(t, err, ErrSessionInvalid)
}
