package imagetemplates

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("image template not found")

type Template struct {
	ID          string
	DisplayName string
	Description string
	ImageRef    string
	DefaultCmd  []string
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) List(ctx context.Context) ([]Template, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, display_name, description, image_ref, default_cmd
		 FROM image_templates WHERE enabled = true ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.DisplayName, &t.Description, &t.ImageRef, &t.DefaultCmd); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) ByID(ctx context.Context, id string) (Template, error) {
	var t Template
	err := s.pool.QueryRow(ctx,
		`SELECT id, display_name, description, image_ref, default_cmd
		 FROM image_templates WHERE id = $1 AND enabled = true`,
		id,
	).Scan(&t.ID, &t.DisplayName, &t.Description, &t.ImageRef, &t.DefaultCmd)
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrNotFound
	}
	if err != nil {
		return Template{}, fmt.Errorf("select: %w", err)
	}
	return t, nil
}
