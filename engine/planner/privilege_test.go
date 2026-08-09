package planner

import (
	"errors"
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// TestValidateRoleMembership is FR-PLAN-10 / AC-12: the check that must fail at
// plan time rather than at leaf 300 of 400.
func TestValidateRoleMembership(t *testing.T) {
	tests := []struct {
		name           string
		build          func() (*FakeCatalog, []Relation)
		wantViolations []string
	}{
		{
			name: "role is a member of the single owner",
			build: func() (*FakeCatalog, []Relation) {
				f := standardTree()
				return f, mustDiscover(t, f).Relations()
			},
		},
		{
			name: "role is not a member of the owner",
			build: func() (*FakeCatalog, []Relation) {
				f := standardTree()
				f.Members[ownerOID] = false
				return f, mustDiscover(t, f).Relations()
			},
			wantViolations: []string{
				"public.orders",
				"public.orders_2026_01",
				"public.orders_2026_02",
				"public.orders_2026_03",
			},
		},
		{
			name: "one leaf was reassigned to another owner",
			build: func() (*FakeCatalog, []Relation) {
				f := standardTree()
				topo := mustDiscover(t, f)
				rels := topo.Relations()
				// orders_2026_02 changed hands after the tree was built.
				for i := range rels {
					if rels[i].Name.Name == "orders_2026_02" {
						rels[i].OwnerOID = strangeOID
					}
				}
				return f, rels
			},
			wantViolations: []string{"public.orders_2026_02"},
		},
		{
			name: "owner OID has no pg_roles row, so it fails closed",
			build: func() (*FakeCatalog, []Relation) {
				f := standardTree()
				rels := mustDiscover(t, f).Relations()
				rels[1].OwnerOID = 4242
				return f, rels
			},
			wantViolations: []string{"public.orders_2026_01"},
		},
		{
			name: "no relations is vacuously satisfied",
			build: func() (*FakeCatalog, []Relation) {
				return standardTree(), nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, rels := tc.build()
			err := ValidateRoleMembership(ctx(), f, f.Role, rels)

			if len(tc.wantViolations) == 0 {
				if err != nil {
					t.Fatalf("ValidateRoleMembership: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ValidateRoleMembership succeeded, want a privilege error")
			}
			var pe *PrivilegeError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *PrivilegeError", err)
			}
			if !errors.Is(err, protocol.ErrInsufficientPrivilege) {
				t.Error("errors.Is(err, ErrInsufficientPrivilege) = false")
			}
			if got := protocol.ExitCodeFor(err); got != protocol.ExitInsufficientPrivilege {
				t.Errorf("exit code = %d, want 16", got)
			}
			if pe.Role != f.Role {
				t.Errorf("Role = %q, want %q", pe.Role, f.Role)
			}
			if len(pe.Violations) != len(tc.wantViolations) {
				t.Fatalf("violations = %+v, want %v", pe.Violations, tc.wantViolations)
			}
			for i, want := range tc.wantViolations {
				if pe.Violations[i].Relation != want {
					t.Errorf("violation %d = %q, want %q", i, pe.Violations[i].Relation, want)
				}
			}
			// The message must name a relation the operator can act on.
			if !strings.Contains(err.Error(), tc.wantViolations[0]) {
				t.Errorf("message %q does not name %q", err.Error(), tc.wantViolations[0])
			}
		})
	}
}

// TestValidateRoleMembershipBatchesOwners guards NFR-PERF-1: 1,000 leaves owned
// by one role must cost one membership query, not 1,000.
func TestValidateRoleMembershipBatchesOwners(t *testing.T) {
	names := make([]string, 0, 300)
	for i := 0; i < 300; i++ {
		names = append(names, "orders_p"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	f := tree(protocol.StrategyRange, names...)
	topo := mustDiscover(t, f)

	before := f.Calls["RoleMemberships"]
	if err := ValidateRoleMembership(ctx(), f, f.Role, topo.Relations()); err != nil {
		t.Fatalf("ValidateRoleMembership: %v", err)
	}
	if got := f.Calls["RoleMemberships"] - before; got != 1 {
		t.Errorf("RoleMemberships called %d times for 301 relations, want 1", got)
	}
}

func TestValidateRoleMembershipPropagatesCatalogErrors(t *testing.T) {
	f := standardTree()
	rels := mustDiscover(t, f).Relations()
	f.Err = ErrCatalogUnavailable.Detailf("connection reset")

	err := ValidateRoleMembership(ctx(), f, f.Role, rels)
	if !errors.Is(err, ErrCatalogUnavailable) {
		t.Fatalf("err = %v, want ErrCatalogUnavailable", err)
	}
	var pe *PrivilegeError
	if errors.As(err, &pe) {
		t.Error("an unreachable catalog was reported as a privilege failure")
	}
}

func TestPrivilegeErrorMessages(t *testing.T) {
	tests := []struct {
		name string
		err  *PrivilegeError
		want []string
	}{
		{
			name: "no violations",
			err:  &PrivilegeError{Role: "migrator"},
			want: []string{"migrator", "owning role"},
		},
		{
			name: "one violation names the owner",
			err: &PrivilegeError{Role: "migrator", Violations: []OwnershipViolation{
				{Relation: "public.orders", Owner: "app_owner", OwnerOID: 10},
			}},
			want: []string{"migrator", "app_owner", "public.orders"},
		},
		{
			name: "many violations report the count and the first",
			err: &PrivilegeError{Role: "migrator", Violations: []OwnershipViolation{
				{Relation: "public.orders", Owner: "app_owner"},
				{Relation: "public.orders_2026_01", Owner: "app_owner"},
			}},
			want: []string{"2 relations", "public.orders"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not contain %q", msg, want)
				}
			}
			if !errors.Is(tc.err, &PrivilegeError{}) {
				t.Error("a *PrivilegeError should match any *PrivilegeError target")
			}
		})
	}
}
