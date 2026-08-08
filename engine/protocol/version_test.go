package protocol

import (
	"errors"
	"strings"
	"testing"
)

// NFR-COMPAT-3: the executor refuses a plan with an unsupported format version.
func TestCheckFormatVersion(t *testing.T) {
	tests := []struct {
		name    string
		version int
		wantErr bool
	}{
		{"current", PlanFormatVersion, false},
		{"zero, which is an unversioned or hand-written file", 0, true},
		{"negative", -1, true},
		{"a future version this binary cannot read", PlanFormatVersion + 1, true},
		{"a far-future version", 9999, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckFormatVersion(tc.version)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckFormatVersion(%d) = %v, wantErr %v", tc.version, err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !errors.Is(err, ErrUnsupportedFormatVersion) {
				t.Fatalf("error %v is not ErrUnsupportedFormatVersion", err)
			}
			// The operator needs to know which version the file declares and
			// which ones this binary accepts.
			if !strings.Contains(err.Error(), "1") {
				t.Errorf("message %q does not name the supported versions", err)
			}
		})
	}
}

func TestSupportedFormatVersions(t *testing.T) {
	versions := SupportedFormatVersions()
	if len(versions) == 0 {
		t.Fatal("no supported versions")
	}
	found := false
	for _, v := range versions {
		if v == PlanFormatVersion {
			found = true
		}
		if !IsSupportedFormatVersion(v) {
			t.Errorf("version %d is listed but IsSupportedFormatVersion says no", v)
		}
	}
	if !found {
		t.Errorf("the version this binary writes (%d) is not in %v", PlanFormatVersion, versions)
	}

	// The returned slice must be a copy: a caller must not be able to widen
	// what this binary accepts.
	versions[0] = 999
	if IsSupportedFormatVersion(999) {
		t.Fatal("SupportedFormatVersions returned the package-level slice")
	}
}

// The vocabulary is a versioned engine contract (TRD §7.2.2). This test is the
// tripwire: adding a node kind must be accompanied by a format-version bump.
//
// The count is pinned directly. It used to be guarded by
// `if PlanFormatVersion == 1`, and M1 set PlanFormatVersion = 2, so the test
// could not fail from that moment on — a tenth kind would have shipped inside
// format version 2 in total silence. The failure that would then reach an
// operator is the wrong one: an older binary accepts the plan (2 is a supported
// version) and dies inside Node.Validate with `"index.detach" is not one of
// [...]`, which reads as a corrupt plan file rather than "upgrade partitionctl"
// (NFR-COMPAT-3).
//
// Update kindsInCurrentFormat and PlanFormatVersion together, in one change.
func TestNodeVocabularyIsPinnedToTheFormatVersion(t *testing.T) {
	const (
		kindsInCurrentFormat = 9
		formatForThatCount   = 2
	)
	if PlanFormatVersion != formatForThatCount {
		t.Fatalf("PlanFormatVersion is %d, but this tripwire pins the kind count for format %d. "+
			"Bumping the format version means re-pinning the count here in the same change",
			PlanFormatVersion, formatForThatCount)
	}
	if len(AllNodeKinds()) != kindsInCurrentFormat {
		t.Fatalf("format version %d defines %d node kinds, found %d. "+
			"Adding a kind is a versioned engine change: bump PlanFormatVersion (TRD §7.2.2)",
			PlanFormatVersion, kindsInCurrentFormat, len(AllNodeKinds()))
	}
}
