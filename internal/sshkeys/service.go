package sshkeys

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"
)

var (
	ErrInvalidKey = errors.New("invalid ssh public key")
	ErrDuplicate  = errors.New("duplicate ssh key")
	ErrNotFound   = errors.New("ssh key not found")
)

type Key struct {
	ID          uuid.UUID
	Name        string
	PublicKey   string
	Fingerprint string
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Add(ctx context.Context, userID uuid.UUID, name, publicKey string) (Key, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return Key{}, ErrInvalidKey
	}
	fp := ssh.FingerprintSHA256(pk)

	var id uuid.UUID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO ssh_keys (user_id, name, public_key, fingerprint)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		userID, name, publicKey, fp,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Key{}, ErrDuplicate
		}
		return Key{}, fmt.Errorf("insert: %w", err)
	}
	return Key{ID: id, Name: name, PublicKey: publicKey, Fingerprint: fp}, nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Key, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, public_key, fingerprint FROM ssh_keys WHERE user_id = $1 ORDER BY created_at`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.Name, &k.PublicKey, &k.Fingerprint); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Service) Delete(ctx context.Context, userID, keyID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM ssh_keys WHERE id = $1 AND user_id = $2`,
		keyID, userID,
	)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
