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
func TestNodeVocabularyIsPinnedToTheFormatVersion(t *testing.T) {
	const kindsInFormatV1 = 9
	if PlanFormatVersion == 1 && len(AllNodeKinds()) != kindsInFormatV1 {
		t.Fatalf("format version 1 defines %d node kinds, found %d. "+
			"Adding a kind is a versioned engine change: bump PlanFormatVersion (TRD §7.2.2)",
			kindsInFormatV1, len(AllNodeKinds()))
	}
}
