//go:build linux

package watchdog

import "testing"

// The comm field of /proc/<pid>/stat is parenthesised and may itself contain
// spaces and parentheses, which is why the parser counts fields from the LAST
// ')'. A process named ") (evil" is the case a naive split gets wrong.
func TestParseProcStatCPU(t *testing.T) {
	// Fields after ')': state ppid pgrp session tty_nr tpgid flags minflt
	// cminflt majflt cmajflt utime stime — utime is index 11, stime 12.
	tail := "S 1 1 1 0 -1 4194304 100 0 0 0 250 50 0 0"

	cases := []struct {
		name    string
		in      string
		want    float64
		wantErr bool
	}{
		{"plain comm", "1234 (cat) " + tail, 3.0, false},
		{"comm with spaces", "1234 (my program) " + tail, 3.0, false},
		{"comm with parens", "1234 (weird ) (name) " + tail, 3.0, false},
		{"no close paren", "1234 cat S 1 1", 0, true},
		{"too few fields", "1234 (cat) S 1 1", 0, true},
		{"non-numeric utime", "1234 (cat) S 1 1 1 0 -1 4194304 100 0 0 0 x 50 0 0", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProcStatCPU(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseProcStatCPU = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProcStatCPU: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseProcStatCPU = %v, want %v", got, tc.want)
			}
		})
	}
}
