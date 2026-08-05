//go:build darwin

package watchdog

import "testing"

func TestParseCPUTime(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"0:00.05", 0.05, false},
		{"1:30.00", 90, false},
		{"01:02:03.00", 3723, false},
		{"2-01:00:00.00", 2*86400 + 3600, false},
		// A malformed sample must be an error, never a silent 0.0 — a 0.0
		// reading would keep a CPU limit from ever firing.
		{"", 0, true},
		{"garbage", 0, true},
		{"1:2:3:4.0", 0, true},
		{"x:00.05", 0, true},
		{"0:zz", 0, true},
	}
	for _, tc := range cases {
		got, err := parseCPUTime(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseCPUTime(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCPUTime(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseCPUTime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
