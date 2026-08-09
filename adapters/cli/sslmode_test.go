package cli

import (
	"strings"
	"testing"
)

// The default connection settings must produce a DSN the shipped driver can
// actually open.
//
// They did not. Defaults() returned sslmode "prefer" -- libpq's own default,
// and a value lib/pq rejects outright. Every documented invocation that did not
// supply a full DSN failed at connect with
//
//	pq: unsupported sslmode "prefer"; only "require" (default), "verify-full",
//	"verify-ca", and "disable" supported
//
// which names the driver, not the flag, so there was no path from the message
// to the fix. The demo harness never caught it because Makefile and demo.sh both
// export a full PARTITIONCTL_DSN with ?sslmode=disable, so the discrete-parameter
// path -- the one the README documents -- was never exercised.
//
// This package may not import a database driver, so these tests cannot open a
// connection. They pin the two things that can be checked offline: the default
// is a value the driver accepts, and Validate rejects the ones it does not.

func TestDefaultSSLModeIsAcceptedByTheShippedDriver(t *testing.T) {
	got := Defaults().SSLMode
	for _, m := range SupportedSSLModes {
		if got == m {
			return
		}
	}
	t.Fatalf("Defaults().SSLMode = %q, which lib/pq rejects at connect time; want one of %v",
		got, SupportedSSLModes)
}

// The mirror of lib/pq's accepted set must not drift into libpq's larger one.
// "prefer" and "allow" are the two libpq modes lib/pq does not implement, and
// "prefer" is exactly the value that shipped broken.
func TestSupportedSSLModesExcludesTheOnesLibpqHasButLibPqDoesNot(t *testing.T) {
	for _, unsupported := range []string{"prefer", "allow"} {
		for _, m := range SupportedSSLModes {
			if m == unsupported {
				t.Errorf("SupportedSSLModes contains %q, which lib/pq rejects", unsupported)
			}
		}
	}
}

func TestValidateRejectsUnsupportedSSLModeWithAnActionableMessage(t *testing.T) {
	c := Defaults()
	c.SSLMode = "prefer"

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil for sslmode \"prefer\"; want a failure before any connection is attempted")
	}

	// The message has to name the knob, not just the bad value: that is the
	// whole reason this check exists rather than letting the driver report it.
	msg := err.Error()
	for _, want := range []string{"prefer", "-sslmode", "PARTITIONCTL_DSN", "disable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Validate() error does not mention %q, so a reader cannot act on it:\n%s", want, msg)
		}
	}
}

func TestValidateAcceptsEverySupportedSSLMode(t *testing.T) {
	for _, m := range SupportedSSLModes {
		c := Defaults()
		c.SSLMode = m
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() rejected supported sslmode %q: %v", m, err)
		}
	}
}

// A full DSN carries its own sslmode, so the discrete field is not consulted and
// must not be second-guessed -- PARTITIONCTL_DSN is the documented escape hatch.
func TestValidateIgnoresSSLModeWhenAFullDSNIsSupplied(t *testing.T) {
	c := Defaults()
	c.DSN = "postgres://u@localhost:5432/db?sslmode=disable"
	c.SSLMode = "prefer"

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() rejected a config carrying an explicit DSN: %v", err)
	}
}
