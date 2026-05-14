package nodes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrTokenInvalid     = errors.New("pair token invalid or expired")
	ErrNodeNameTaken    = errors.New("node name already taken for this user")
	ErrNodeTokenInvalid = errors.New("node token invalid")
	ErrNotFound         = errors.New("node not found")
)

const (
	pairTokenPrefix = "FX-"
	pairTokenBytes  = 48 // 48 bytes → 64 base64 chars + "FX-" prefix = 67 total chars
	nodeTokenBytes  = 32
	pairTokenTTL    = 10 * time.Minute
)

type Node struct {
	ID           uuid.UUID
	OwnerUserID  uuid.UUID
	Name         string
	AgentVersion string
	GPUInfo      []byte
	Status       string
	LastSeenAt   *time.Time
	CreatedAt    time.Time
}

type PairRequest struct {
	Token   string
	Name    string
	GPUInfo []byte // optional, agent may not have GPU info at pair time
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// PoolFor returns the underlying pgx pool for test wiring. Production code
// should call methods on Service directly.
func PoolFor(s *Service) *pgxpool.Pool { return s.pool }

func (s *Service) CreatePairToken(ctx context.Context, userID uuid.UUID) (string, error) {
	raw := make([]byte, pairTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	token := pairTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash := hashToken(token)

	_, err := s.pool.Exec(ctx,
		`INSERT INTO pair_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hash, userID, time.Now().Add(pairTokenTTL),
	)
	if err != nil {
		return "", fmt.Errorf("insert pair token: %w", err)
	}
	return token, nil
}

// HashTokenForTest exposes hashing for test fixtures. Not used in production paths.
func (s *Service) HashTokenForTest(token string) (string, error) {
	return hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) PairNode(ctx context.Context, req PairRequest) (Node, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Node{}, "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock + validate token
	var (
		userID    uuid.UUID
		expiresAt time.Time
		usedAt    *time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT user_id, expires_at, used_at FROM pair_tokens
		 WHERE token_hash = $1 FOR UPDATE`,
		hashToken(req.Token),
	).Scan(&userID, &expiresAt, &usedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, "", ErrTokenInvalid
	}
	if err != nil {
		return Node{}, "", fmt.Errorf("select pair token: %w", err)
	}
	if usedAt != nil || time.Now().After(expiresAt) {
		return Node{}, "", ErrTokenInvalid
	}

	// Mark used
	_, err = tx.Exec(ctx,
		`UPDATE pair_tokens SET used_at = now() WHERE token_hash = $1`,
		hashToken(req.Token),
	)
	if err != nil {
		return Node{}, "", fmt.Errorf("mark token used: %w", err)
	}

	// Generate node token
	nodeRaw := make([]byte, nodeTokenBytes)
	if _, err := rand.Read(nodeRaw); err != nil {
		return Node{}, "", fmt.Errorf("read random: %w", err)
	}
	nodeToken := base64.RawURLEncoding.EncodeToString(nodeRaw)
	nodeTokenHash := hashToken(nodeToken)

	// Insert node
	gpuInfo := req.GPUInfo
	if len(gpuInfo) == 0 {
		gpuInfo = []byte(`[]`)
	}
	var nodeID uuid.UUID
	err = tx.QueryRow(ctx,
		`INSERT INTO nodes (owner_user_id, name, gpu_info, node_token_hash)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		userID, req.Name, gpuInfo, nodeTokenHash,
	).Scan(&nodeID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Node{}, "", ErrNodeNameTaken
		}
		return Node{}, "", fmt.Errorf("insert node: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Node{}, "", fmt.Errorf("commit: %w", err)
	}
	return Node{
		ID:          nodeID,
		OwnerUserID: userID,
		Name:        req.Name,
		GPUInfo:     gpuInfo,
		Status:      "offline",
	}, nodeToken, nil
}

func (s *Service) AuthenticateNodeToken(ctx context.Context, token string) (Node, error) {
	if token == "" {
		return Node{}, ErrNodeTokenInvalid
	}
	hash := hashToken(token)
	var n Node
	err := s.pool.QueryRow(ctx,
		`SELECT id, owner_user_id, name, agent_version, gpu_info, status, last_seen_at
		 FROM nodes WHERE node_token_hash = $1`,
		hash,
	).Scan(&n.ID, &n.OwnerUserID, &n.Name, &n.AgentVersion, &n.GPUInfo, &n.Status, &n.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, ErrNodeTokenInvalid
	}
	if err != nil {
		return Node{}, fmt.Errorf("select node: %w", err)
	}
	return n, nil
}

func (s *Service) UpdateRegister(ctx context.Context, nodeID uuid.UUID, agentVersion string, gpuInfo []byte) error {
	if len(gpuInfo) == 0 {
		gpuInfo = []byte(`[]`)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE nodes
		 SET agent_version = $2, gpu_info = $3, status = 'online', last_seen_at = now()
		 WHERE id = $1`,
		nodeID, agentVersion, gpuInfo,
	)
	if err != nil {
		return fmt.Errorf("update register: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) RecordHeartbeat(ctx context.Context, nodeID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE nodes SET status = 'online', last_seen_at = now() WHERE id = $1`,
		nodeID,
	)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) MarkOffline(ctx context.Context, nodeID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE nodes SET status = 'offline' WHERE id = $1`,
		nodeID,
	)
	if err != nil {
		return fmt.Errorf("mark offline: %w", err)
	}
	return nil
}

func (s *Service) ListByOwner(ctx context.Context, userID uuid.UUID) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_user_id, name, agent_version, gpu_info, status, last_seen_at, created_at
		FROM nodes WHERE owner_user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.OwnerUserID, &n.Name, &n.AgentVersion, &n.GPUInfo,
			&n.Status, &n.LastSeenAt, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
