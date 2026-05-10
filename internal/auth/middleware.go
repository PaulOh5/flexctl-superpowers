package auth

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/paul/flexctl/internal/httperr"
)

type ctxKey int

const userIDKey ctxKey = 1

const cookieName = "flex_session"

func RequireSession(signer *SessionSigner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(cookieName)
			if err != nil {
				httperr.Write(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			sess, err := signer.Decode(c.Value)
			if err != nil {
				httperr.Write(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			ctx := context.WithValue(r.Context(), userIDKey, sess.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func UserIDFrom(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(userIDKey).(uuid.UUID)
	return v, ok
}
