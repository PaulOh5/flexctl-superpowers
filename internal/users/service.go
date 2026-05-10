package users

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paul/flexctl/internal/auth"
)

var (
	ErrEmailTaken       = errors.New("email already registered")
	ErrSlugTaken        = errors.New("slug already taken")
	ErrInvalidEmail     = errors.New("invalid email")
	ErrInvalidSlug      = errors.New("invalid slug")
	ErrPasswordTooShort = errors.New("password too short")
	ErrNotFound         = errors.New("user not found")
	ErrBadCredentials   = errors.New("bad credentials")
)

type User struct {
	ID    uuid.UUID
	Email string
	Slug  string
}

var slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

const minPasswordLen = 12

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

func (s *Service) Signup(ctx context.Context, email, slug, password string) (User, error) {
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return User{}, ErrInvalidEmail
	}
	emailNorm := strings.ToLower(addr.Address)

	slug = strings.ToLower(strings.TrimSpace(slug))
	if !slugRe.MatchString(slug) {
		return User{}, ErrInvalidSlug
	}

	if len(password) < minPasswordLen {
		return User{}, ErrPasswordTooShort
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("hash: %w", err)
	}

	var id uuid.UUID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users (email, slug, password_hash) VALUES ($1, $2, $3) RETURNING id`,
		emailNorm, slug, hash,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err, "users_email_key") {
			return User{}, ErrEmailTaken
		}
		if isUniqueViolation(err, "users_slug_key") {
			return User{}, ErrSlugTaken
		}
		return User{}, fmt.Errorf("insert: %w", err)
	}
	return User{ID: id, Email: emailNorm, Slug: slug}, nil
}

func (s *Service) Authenticate(ctx context.Context, email, password string) (User, error) {
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return User{}, ErrBadCredentials
	}
	emailNorm := strings.ToLower(addr.Address)

	var (
		id   uuid.UUID
		slug string
		hash string
	)
	err = s.pool.QueryRow(ctx,
		`SELECT id, slug, password_hash FROM users WHERE lower(email) = $1`,
		emailNorm,
	).Scan(&id, &slug, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrBadCredentials
	}
	if err != nil {
		return User{}, fmt.Errorf("select: %w", err)
	}
	if err := auth.VerifyPassword(hash, password); err != nil {
		return User{}, ErrBadCredentials
	}
	return User{ID: id, Email: emailNorm, Slug: slug}, nil
}

func (s *Service) ByID(ctx context.Context, id uuid.UUID) (User, error) {
	var u User
	u.ID = id
	err := s.pool.QueryRow(ctx,
		`SELECT email, slug FROM users WHERE id = $1`, id,
	).Scan(&u.Email, &u.Slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("select: %w", err)
	}
	return u, nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23505" {
		return false
	}
	return pgErr.ConstraintName == constraint
}
