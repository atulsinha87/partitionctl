package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTimestampMarshalIsCanonical(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"utc whole second", time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC), `"2026-08-07T12:00:00Z"`},
		{"offset normalizes to utc", time.Date(2026, 8, 7, 17, 30, 0, 0, time.FixedZone("IST", 5*3600+1800)), `"2026-08-07T12:00:00Z"`},
		{"negative offset", time.Date(2026, 8, 7, 8, 0, 0, 0, time.FixedZone("EDT", -4*3600)), `"2026-08-07T12:00:00Z"`},
		{"milliseconds", time.Date(2026, 8, 7, 12, 0, 0, 100000000, time.UTC), `"2026-08-07T12:00:00.1Z"`},
		{"nanoseconds", time.Date(2026, 8, 7, 12, 0, 0, 123456789, time.UTC), `"2026-08-07T12:00:00.123456789Z"`},
		{"zero value", time.Time{}, `"0001-01-01T00:00:00Z"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(NewTimestamp(tc.in))
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("Marshal = %s, want %s", got, tc.want)
			}
		})
	}
}

// The same instant in different zones must produce the same bytes, which is
// what keeps the digest zone-independent.
func TestTimestampZoneNormalization(t *testing.T) {
	instant := time.Date(2026, 8, 7, 12, 0, 0, 500000000, time.UTC)
	zones := []*time.Location{
		time.UTC,
		time.FixedZone("IST", 5*3600+1800),
		time.FixedZone("EDT", -4*3600),
		time.FixedZone("NPT", 5*3600+2700),
		time.Local,
	}
	var want []byte
	for i, loc := range zones {
		got, err := json.Marshal(NewTimestamp(instant.In(loc)))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if i == 0 {
			want = got
			continue
		}
		if string(got) != string(want) {
			t.Fatalf("zone %s produced %s, want %s", loc, got, want)
		}
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	instants := []time.Time{
		time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 7, 12, 0, 0, 1, time.UTC),
		time.Date(2026, 8, 7, 12, 0, 0, 999999999, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Time{},
	}
	for _, want := range instants {
		encoded, err := json.Marshal(NewTimestamp(want))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var got Timestamp
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", encoded, err)
		}
		if !got.Time.Equal(want) {
			t.Fatalf("round trip: %s != %s", got.Time, want)
		}
		again, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("Marshal (second): %v", err)
		}
		if string(again) != string(encoded) {
			t.Fatalf("re-encoding is not a fixed point: %s vs %s", encoded, again)
		}
	}
}

func TestTimestampUnmarshalAcceptsAnyRFC3339(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
	}{
		{`"2026-08-07T12:00:00Z"`, time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)},
		{`"2026-08-07T17:30:00+05:30"`, time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)},
		{`"2026-08-07T08:00:00-04:00"`, time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)},
		{`"2026-08-07T12:00:00.000Z"`, time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)},
		{`null`, time.Time{}},
	}
	for _, tc := range tests {
		var got Timestamp
		if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tc.in, err)
		}
		if !got.Time.Equal(tc.want) {
			t.Errorf("Unmarshal(%s) = %s, want %s", tc.in, got.Time, tc.want)
		}
		// Whatever the input offset was, the stored value is UTC.
		if tc.in != "null" && got.Time.Location() != time.UTC {
			t.Errorf("Unmarshal(%s) kept location %s", tc.in, got.Time.Location())
		}
	}
}

func TestTimestampUnmarshalRejectsGarbage(t *testing.T) {
	for _, in := range []string{`"not a time"`, `"2026-08-07"`, `12345`, `{}`, `"2026-13-01T00:00:00Z"`} {
		var ts Timestamp
		if err := json.Unmarshal([]byte(in), &ts); err == nil {
			t.Errorf("Unmarshal(%s) accepted garbage", in)
		}
	}
}

func TestTimestampCanonicalAndString(t *testing.T) {
	ts := NewTimestamp(time.Date(2026, 8, 7, 17, 30, 0, 0, time.FixedZone("IST", 5*3600+1800)))
	if got := ts.Canonical(); got != "2026-08-07T12:00:00Z" {
		t.Errorf("Canonical() = %q", got)
	}
	if got := ts.String(); got != ts.Canonical() {
		t.Errorf("String() = %q, want %q", got, ts.Canonical())
	}
	// The embedded time.Time API stays available.
	if ts.Year() != 2026 {
		t.Errorf("Year() = %d", ts.Year())
	}
	if !NewTimestamp(time.Time{}).IsZero() {
		t.Error("IsZero() on the zero value")
	}
}

func TestNowIsUsable(t *testing.T) {
	before := time.Now().Add(-time.Second)
	ts := Now()
	after := time.Now().Add(time.Second)
	if ts.Time.Before(before) || ts.Time.After(after) {
		t.Fatalf("Now() = %s, outside [%s, %s]", ts, before, after)
	}
	// It must serialize without a monotonic reading leaking in.
	if _, err := json.Marshal(ts); err != nil {
		t.Fatalf("Marshal: %v", err)
	}
}
