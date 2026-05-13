package devices

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paul/flexctl/internal/headscale"
	"github.com/paul/flexctl/internal/users"
)

var (
	ErrInvalidName = errors.New("invalid device name")
	ErrConflict    = errors.New("device hostname already taken")
	ErrNotFound    = errors.New("device not found")
)

// nameRe requires at least 2 chars: start and end must be [a-z0-9], middle chars optional.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`)

// Device is the DB representation of a paired device.
type Device struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Name       string
	Hostname   string
	CreatedAt  time.Time
	LastSeenAt *time.Time
}

// PairResult is returned by Service.Pair.
type PairResult struct {
	Device        Device
	PreauthKey    string
	HeadscaleURL  string
	TailnetDomain string
}

// HeadscaleClient is the subset of headscale.Client that devices.Service needs.
type HeadscaleClient interface {
	CreatePreAuthKey(ctx context.Context, req headscale.PreAuthKeyRequest) (headscale.PreAuthKey, error)
	ListNodes(ctx context.Context, user string) ([]headscale.Node, error)
	DeleteNode(ctx context.Context, id string) error
}

// ServiceConfig holds static configuration for Service.
type ServiceConfig struct {
	HeadscaleClientURL string
	TailnetDomain      string
}

// Service manages device pairing and lifecycle.
type Service struct {
	pool  *pgxpool.Pool
	hs    HeadscaleClient
	users *users.Service
	cfg   ServiceConfig
}

// NewService constructs a new devices.Service.
func NewService(pool *pgxpool.Pool, hs HeadscaleClient, usersSvc *users.Service, cfg ServiceConfig) *Service {
	return &Service{pool: pool, hs: hs, users: usersSvc, cfg: cfg}
}

// Pair creates (or upserts) a device row and returns a fresh Headscale pre-auth key.
func (s *Service) Pair(ctx context.Context, userID uuid.UUID, name string) (PairResult, error) {
	if !nameRe.MatchString(name) {
		return PairResult{}, ErrInvalidName
	}

	u, err := s.users.ByID(ctx, userID)
	if err != nil {
		return PairResult{}, fmt.Errorf("user: %w", err)
	}
	hostname := u.Slug + "-device-" + name

	var d Device
	err = s.pool.QueryRow(ctx, `
		INSERT INTO devices (user_id, name, hostname)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, name) DO UPDATE
		  SET last_seen_at = now()
		RETURNING id, user_id, name, hostname, created_at, last_seen_at`,
		userID, name, hostname,
	).Scan(&d.ID, &d.UserID, &d.Name, &d.Hostname, &d.CreatedAt, &d.LastSeenAt)
	if err != nil {
		// ErrConflict is unreachable in practice: hostname uniqueness follows
		// from slug uniqueness, and (user_id, name) collisions are absorbed by
		// the ON CONFLICT clause above. Kept as defense-in-depth.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return PairResult{}, ErrConflict
		}
		return PairResult{}, fmt.Errorf("upsert device: %w", err)
	}

	pak, err := s.hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User:       u.Slug,
		Reusable:   false,
		Ephemeral:  false,
		Expiration: 10 * time.Minute,
		ACLTags:    []string{"tag:device-" + u.Slug},
	})
	if err != nil {
		return PairResult{}, fmt.Errorf("preauthkey: %w", err)
	}

	return PairResult{
		Device:        d,
		PreauthKey:    pak.Key,
		HeadscaleURL:  s.cfg.HeadscaleClientURL,
		TailnetDomain: s.cfg.TailnetDomain,
	}, nil
}

// List returns all devices owned by userID, ordered by creation time.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Device, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, name, hostname, created_at, last_seen_at
		FROM devices WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.Hostname, &d.CreatedAt, &d.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ByID fetches a single device by its UUID. Returns ErrNotFound if not present.
func (s *Service) ByID(ctx context.Context, id uuid.UUID) (Device, error) {
	var d Device
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, name, hostname, created_at, last_seen_at
		FROM devices WHERE id = $1`, id,
	).Scan(&d.ID, &d.UserID, &d.Name, &d.Hostname, &d.CreatedAt, &d.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("select: %w", err)
	}
	return d, nil
}

// Delete removes the device from the DB and best-effort removes the matching
// Headscale node. Returns ErrNotFound if the device doesn't belong to userID.
func (s *Service) Delete(ctx context.Context, userID, deviceID uuid.UUID) error {
	d, err := s.ByID(ctx, deviceID)
	if err != nil {
		return err
	}
	if d.UserID != userID {
		return ErrNotFound
	}

	u, err := s.users.ByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("user: %w", err)
	}

	// Headscale cleanup is best-effort. The DB row is authoritative; if
	// ListNodes or DeleteNode fails, any orphan Headscale node will be
	// reconciled on the user's next `flexctl login` (which re-pairs and
	// supersedes stale entries by hostname).
	nodes, err := s.hs.ListNodes(ctx, u.Slug)
	if err == nil {
		for _, n := range nodes {
			if n.Name == d.Hostname || n.GivenName == d.Hostname {
				_ = s.hs.DeleteNode(ctx, n.ID)
			}
		}
	}

	tag, err := s.pool.Exec(ctx,
		`DELETE FROM devices WHERE id = $1 AND user_id = $2`, deviceID, userID)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
