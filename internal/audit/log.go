package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	UserID   *uuid.UUID
	Action   string
	Target   string
	Metadata map[string]any
	IP       net.IP
}

func Log(ctx context.Context, pool *pgxpool.Pool, e Event) error {
	var meta []byte
	if e.Metadata != nil {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
		meta = b
	}
	var ipStr *string
	if e.IP != nil {
		s := e.IP.String()
		ipStr = &s
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO audit_log (user_id, action, target, metadata, ip)
		 VALUES ($1, $2, $3, $4, $5)`,
		e.UserID, e.Action, e.Target, meta, ipStr,
	)
	if err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}
