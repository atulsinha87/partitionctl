package planner

import (
	"context"
	"sort"
)

// ValidateRoleMembership checks that role has the privileges of the owning role
// of every relation the plan would modify (FR-PLAN-10, AC-12).
//
// This is the one privilege check that cannot be deferred. CREATE INDEX
// CONCURRENTLY requires ownership of the table, superuser is unavailable on
// managed PostgreSQL (§11.2), and a build that discovers this at leaf 300 of
// 400 has already burned hours. So it runs at plan time and produces no plan at
// all when it fails.
//
// The test is "has the privileges of", not "is listed as a member of". A role
// that is a NOINHERIT member could reach the owner's privileges with SET ROLE,
// but PartitionCTL never issues SET ROLE, so a NOINHERIT membership would pass
// the check and then fail the DDL. Superusers pass, because PostgreSQL treats
// them as holding every role.
//
// It returns a *[PrivilegeError], which maps to exit code 16.
func ValidateRoleMembership(ctx context.Context, cr CatalogReader, role string, rels []Relation) error {
	if len(rels) == 0 {
		return nil
	}

	owners := make([]uint32, 0, len(rels))
	seen := make(map[uint32]struct{}, len(rels))
	for _, r := range rels {
		if _, dup := seen[r.OwnerOID]; dup {
			continue
		}
		seen[r.OwnerOID] = struct{}{}
		owners = append(owners, r.OwnerOID)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })

	memberships, err := cr.RoleMemberships(ctx, role, owners)
	if err != nil {
		return err
	}

	var violations []OwnershipViolation
	for _, r := range rels {
		m, known := memberships[r.OwnerOID]
		if known && m.IsMember {
			continue
		}
		// An owner OID with no pg_roles row cannot be a role this session is a
		// member of, so an unresolved owner is a violation, not a pass. Failing
		// closed is the only safe direction here.
		violations = append(violations, OwnershipViolation{
			Relation: r.Name.String(),
			Owner:    m.OwnerName,
			OwnerOID: r.OwnerOID,
		})
	}
	if len(violations) > 0 {
		return &PrivilegeError{Role: role, Violations: violations}
	}
	return nil
}
