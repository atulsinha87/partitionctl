package protocol

import "testing"

// The String forms end up in logs, audit rows and operator messages, so they
// are part of the contract downstream packages read.
func TestStringForms(t *testing.T) {
	tests := []struct {
		got  string
		want string
	}{
		{PlanID("plan-1").String(), "plan-1"},
		{NodeID("assert").String(), "assert"},
		{OpCreateIndex.String(), "create-index"},
		{KindIndexCreateConcurrently.String(), "index.create_concurrently"},
		{AuthProvenance.String(), "provenance"},
		{NodeRetryWait.String(), "RETRY_WAIT"},
		{ExitDigestMismatch.String(), "10"},
		{ExitSuccess.String(), "0"},
		{RelationState{Schema: "public", Name: "orders"}.String(), "public.orders"},
		{RelationState{Name: "orders"}.String(), "orders"},
		{TopologyChange{Change: TopologyPartitionAdded, Relation: "public.orders_2026_03"}.String(),
			"partition_added public.orders_2026_03"},
		{TopologyChange{Change: TopologyStrategyChanged, Detail: "planned RANGE, live LIST"}.String(),
			"strategy_changed: planned RANGE, live LIST"},
		{TopologyChange{Change: TopologyRootChanged}.String(), "root_changed"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestComputeTopologyFingerprintMatchesTheMethod(t *testing.T) {
	top := sampleTopology()
	viaFunc, err := ComputeTopologyFingerprint(top)
	if err != nil {
		t.Fatalf("ComputeTopologyFingerprint: %v", err)
	}
	viaMethod, err := top.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if viaFunc != viaMethod {
		t.Fatalf("%s != %s", viaFunc, viaMethod)
	}
}

func TestEnumValidity(t *testing.T) {
	assertions := []AssertionKind{
		AssertRelationIsPartitioned, AssertPartitionStrategy, AssertPartitionDepth,
		AssertNoDefaultPartition, AssertRoleMembership, AssertIndexNameAvailable,
		AssertIndexExists, AssertIndexIsPartitioned, AssertIndexNotConstraintBacked,
		AssertLeavesAttached,
	}
	for _, a := range assertions {
		if !a.Valid() {
			t.Errorf("assertion %q is not valid", a)
		}
	}
	if AssertionKind("looks_right").Valid() {
		t.Error("an unknown assertion reported valid")
	}

	checks := []VerifyCheckKind{
		CheckIndexValid, CheckIndexAttached, CheckParentIndexValid,
		CheckLeafIndexCount, CheckIndexAbsent, CheckNoLeftoverIndexes,
	}
	for _, c := range checks {
		if !c.Valid() {
			t.Errorf("check %q is not valid", c)
		}
	}
	if VerifyCheckKind("index_probably_fine").Valid() {
		t.Error("an unknown check reported valid")
	}

	reasons := []DropReason{DropInvalidBuild, DropCCNew, DropCCOld, DropUnattachedOrphan}
	for _, r := range reasons {
		if !r.Valid() {
			t.Errorf("reason %q is not valid", r)
		}
	}
	if DropReason("housekeeping").Valid() {
		t.Error("an unknown drop reason reported valid")
	}
}
