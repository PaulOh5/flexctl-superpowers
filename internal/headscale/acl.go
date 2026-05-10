package headscale

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

var ErrInvalidSlug = errors.New("invalid slug for ACL generation")

// slugRe must match the slug regex enforced in users package.
var slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

type aclPolicy struct {
	TagOwners map[string][]string `json:"tagOwners"`
	ACLs      []aclRule           `json:"acls"`
}

type aclRule struct {
	Action string   `json:"action"`
	Src    []string `json:"src"`
	Dst    []string `json:"dst"`
}

// GenerateACL produces a Headscale-compatible policy JSON for the given user slugs.
// For each slug X, generates:
//   - tagOwners["tag:device-X"] = ["control-plane"]
//   - tagOwners["tag:env-X"]    = ["control-plane"]
//   - one ACL: device-X → env-X:22
//
// Output is deterministic (slugs sorted ascending).
func GenerateACL(slugs []string) (string, error) {
	for _, s := range slugs {
		if !slugRe.MatchString(s) {
			return "", fmt.Errorf("%w: %q", ErrInvalidSlug, s)
		}
	}
	sorted := append([]string(nil), slugs...)
	sort.Strings(sorted)

	policy := aclPolicy{
		TagOwners: make(map[string][]string, 2*len(sorted)),
		ACLs:      make([]aclRule, 0, len(sorted)),
	}
	if len(sorted) == 0 {
		// keep empty containers (not nil) so JSON shape stays {tagOwners:{}, acls:[]}
		policy.TagOwners = map[string][]string{}
	}
	for _, s := range sorted {
		policy.TagOwners["tag:device-"+s] = []string{"control-plane"}
		policy.TagOwners["tag:env-"+s] = []string{"control-plane"}
		policy.ACLs = append(policy.ACLs, aclRule{
			Action: "accept",
			Src:    []string{"tag:device-" + s},
			Dst:    []string{"tag:env-" + s + ":22"},
		})
	}

	buf, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal acl: %w", err)
	}
	return string(buf), nil
}
