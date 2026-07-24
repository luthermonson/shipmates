//go:build darwin

package watchdog

import "testing"

func TestParseCPUTime(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"0:01.23", 1.23, true},
		{"2:30.00", 150.0, true},
		{"1:02:03.50", 3723.5, true},
		{"1-01:02:03.00", 90123.0, true},
		{"", 0, false},
		{"garbage", 0, false},
		{"xx:10.0", 0, false},       // bad minutes must be an error, not 0.0
		{"aa:bb:10.0", 0, false},    // bad hours must be an error
		{"1:02:zz.0", 0, false},     // bad seconds must be an error
		{"nope-01:02:03.0", 0, false},
	} {
		got, err := parseCPUTime(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("parseCPUTime(%q) error: %v", tc.in, err)
			} else if got != tc.want {
				t.Errorf("parseCPUTime(%q) = %v, want %v", tc.in, got, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("parseCPUTime(%q) = %v, want error", tc.in, got)
		}
	}
}
