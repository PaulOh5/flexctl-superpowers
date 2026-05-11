package envs

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidName  = errors.New("invalid env name")
	ErrNameTaken    = errors.New("env name already taken")
	ErrNotFound     = errors.New("env not found")
	ErrInvalidState = errors.New("invalid state transition")
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

type Env struct {
	ID                 uuid.UUID
	OwnerUserID        uuid.UUID
	NodeID             uuid.UUID
	TemplateID         string
	Name               string
	Hostname           string
	Status             string
	StatusMessage      string
	SidecarContainerID string
	DevContainerID     string
	GPURequest         int32
	GPUIndices         []int32
	VolumeName         string
}

type CreateRequest struct {
	OwnerUserID uuid.UUID
	NodeID      uuid.UUID
	TemplateID  string
	Name        string
	GPURequest  int32
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Create(ctx context.Context, req CreateRequest) (Env, error) {
	if !nameRe.MatchString(req.Name) {
		return Env{}, ErrInvalidName
	}
	var hostname string
	err := s.pool.QueryRow(ctx,
		`SELECT slug || '-' || $2 FROM users WHERE id = $1`,
		req.OwnerUserID, req.Name,
	).Scan(&hostname)
	if err != nil {
		return Env{}, fmt.Errorf("compute hostname: %w", err)
	}

	var id uuid.UUID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO envs (owner_user_id, node_id, template_id, name, hostname,
		                   gpu_request, volume_name)
		 VALUES ($1, $2, $3, $4, $5, $6, '')
		 RETURNING id`,
		req.OwnerUserID, req.NodeID, req.TemplateID, req.Name, hostname, req.GPURequest,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Env{}, ErrNameTaken
		}
		return Env{}, fmt.Errorf("insert: %w", err)
	}
	volumeName := "flex-env-" + id.String()
	_, err = s.pool.Exec(ctx, `UPDATE envs SET volume_name = $2 WHERE id = $1`, id, volumeName)
	if err != nil {
		return Env{}, fmt.Errorf("set volume: %w", err)
	}
	return Env{
		ID:          id,
		OwnerUserID: req.OwnerUserID,
		NodeID:      req.NodeID,
		TemplateID:  req.TemplateID,
		Name:        req.Name,
		Hostname:    hostname,
		Status:      "creating",
		GPURequest:  req.GPURequest,
		VolumeName:  volumeName,
	}, nil
}

func (s *Service) ByID(ctx context.Context, id uuid.UUID) (Env, error) {
	var e Env
	err := s.pool.QueryRow(ctx, `
		SELECT id, owner_user_id, node_id, template_id, name, hostname,
		       status, status_message, sidecar_container_id, dev_container_id,
		       gpu_request, gpu_indices, volume_name
		FROM envs WHERE id = $1`, id,
	).Scan(&e.ID, &e.OwnerUserID, &e.NodeID, &e.TemplateID, &e.Name, &e.Hostname,
		&e.Status, &e.StatusMessage, &e.SidecarContainerID, &e.DevContainerID,
		&e.GPURequest, &e.GPUIndices, &e.VolumeName)
	if errors.Is(err, pgx.ErrNoRows) {
		return Env{}, ErrNotFound
	}
	if err != nil {
		return Env{}, fmt.Errorf("select: %w", err)
	}
	return e, nil
}

func (s *Service) ListByOwner(ctx context.Context, userID uuid.UUID) ([]Env, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_user_id, node_id, template_id, name, hostname,
		       status, status_message, sidecar_container_id, dev_container_id,
		       gpu_request, gpu_indices, volume_name
		FROM envs WHERE owner_user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []Env
	for rows.Next() {
		var e Env
		if err := rows.Scan(&e.ID, &e.OwnerUserID, &e.NodeID, &e.TemplateID, &e.Name, &e.Hostname,
			&e.Status, &e.StatusMessage, &e.SidecarContainerID, &e.DevContainerID,
			&e.GPURequest, &e.GPUIndices, &e.VolumeName); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) ListRunningOnNode(ctx context.Context, nodeID uuid.UUID) ([]Env, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_user_id, node_id, template_id, name, hostname,
		       status, status_message, sidecar_container_id, dev_container_id,
		       gpu_request, gpu_indices, volume_name
		FROM envs WHERE node_id = $1 AND status = 'running'`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []Env
	for rows.Next() {
		var e Env
		if err := rows.Scan(&e.ID, &e.OwnerUserID, &e.NodeID, &e.TemplateID, &e.Name, &e.Hostname,
			&e.Status, &e.StatusMessage, &e.SidecarContainerID, &e.DevContainerID,
			&e.GPURequest, &e.GPUIndices, &e.VolumeName); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) MarkStatus(ctx context.Context, id uuid.UUID, status string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE envs SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) MarkRunning(ctx context.Context, id uuid.UUID, sidecarID, devID string, gpuIdx []int) error {
	idx32 := make([]int32, 0, len(gpuIdx))
	for _, i := range gpuIdx {
		idx32 = append(idx32, int32(i))
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE envs
		SET status = 'running', status_message = '',
		    sidecar_container_id = $2, dev_container_id = $3, gpu_indices = $4,
		    updated_at = now()
		WHERE id = $1`,
		id, sidecarID, devID, idx32)
	if err != nil {
		return fmt.Errorf("mark running: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) MarkStopped(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE envs
		SET status = 'stopped', gpu_indices = '{}', updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("mark stopped: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) MarkError(ctx context.Context, id uuid.UUID, stage, detail string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE envs
		SET status = 'error', status_message = $2, gpu_indices = '{}', updated_at = now()
		WHERE id = $1`, id, fmt.Sprintf("[%s] %s", stage, detail))
	if err != nil {
		return fmt.Errorf("mark error: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM envs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
