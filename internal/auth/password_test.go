package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct-horse")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(hash, "$argon2id$"))

	require.NoError(t, VerifyPassword(hash, "correct-horse"))
	require.ErrorIs(t, VerifyPassword(hash, "battery-staple"), ErrPasswordMismatch)
}

func TestHashIsRandomSalted(t *testing.T) {
	h1, _ := HashPassword("same-input")
	h2, _ := HashPassword("same-input")
	require.NotEqual(t, h1, h2, "salt이 다르므로 해시도 달라야 함")
}

func TestVerifyRejectsMalformed(t *testing.T) {
	require.ErrorIs(t, VerifyPassword("not-a-real-hash", "x"), ErrInvalidHashFormat)
}
