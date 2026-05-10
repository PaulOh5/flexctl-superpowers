package policy

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/paul/flexctl/internal/headscale"
)

// HeadscaleClient is the subset of headscale.Client policy needs.
type HeadscaleClient interface {
	ListUsers(ctx context.Context) ([]headscale.User, error)
	CreateUser(ctx context.Context, name string) (headscale.User, error)
	DeleteUser(ctx context.Context, name string) error
	SetPolicy(ctx context.Context, hujson string) error
}

type Policy struct {
	pool *pgxpool.Pool
	hs   HeadscaleClient
}

func New(pool *pgxpool.Pool, hs HeadscaleClient) *Policy {
	return &Policy{pool: pool, hs: hs}
}

// emptyPolicy is the minimal Headscale-valid policy pushed when no users exist.
// Headscale 0.23.x rejects an empty acls list ("empty policy" error), so we
// include one wildcard-deny style rule using a defined owner tag as placeholder.
// The tag:placeholder src/dst pair requires tagOwners to be set — we define it
// owned by "control-plane" so the rule validates, but matches no real devices.
const emptyPolicy = `{
  "tagOwners": {
    "tag:placeholder": ["control-plane"]
  },
  "acls": [
    {"action": "accept", "src": ["tag:placeholder"], "dst": ["tag:placeholder:0"]}
  ]
}`

// Refresh reads all user slugs from the DB, regenerates the ACL policy, and
// pushes it to Headscale. Idempotent and safe to call repeatedly.
func (p *Policy) Refresh(ctx context.Context) error {
	slugs, err := p.allSlugs(ctx)
	if err != nil {
		return fmt.Errorf("read slugs: %w", err)
	}

	var policyJSON string
	if len(slugs) == 0 {
		policyJSON = emptyPolicy
	} else {
		policyJSON, err = headscale.GenerateACL(slugs)
		if err != nil {
			return fmt.Errorf("generate acl: %w", err)
		}
	}

	if err := p.hs.SetPolicy(ctx, policyJSON); err != nil {
		return fmt.Errorf("push policy: %w", err)
	}
	return nil
}

func (p *Policy) allSlugs(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT slug FROM users ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var slugs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		slugs = append(slugs, s)
	}
	return slugs, rows.Err()
}
