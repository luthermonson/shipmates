//go:build darwin

package watchdog

import "testing"

// sampleTree* on macOS snapshot every process with its pgid and sum the rows in
// the child's group. These parsers are the group-membership + summation core,
// exercised here on synthetic `ps -Ao pgid=,rss=` / `pgid=,time=` output.
func TestSumGroupRSSFromPS(t *testing.T) {
	// pgid rss(KiB), one process per line, columns as `ps -Ao pgid=,rss=`.
	out := "  1000    100\n" + // root, in the group
		"  1000  40000\n" + // a hogging child in the group
		"  2000  99999\n" + // some other group
		"  bad   12345\n" // unparseable pgid column: skip, not fatal
	total, matched, err := sumGroupRSSFromPS(1000, out)
	if err != nil {
		t.Fatalf("sumGroupRSSFromPS: %v", err)
	}
	if matched != 2 {
		t.Errorf("matched = %d, want 2", matched)
	}
	if want := int64((100 + 40000) * 1024); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}

	// A malformed rss for a row IN the group is an error (skip the tick), not
	// an undercount.
	if _, _, err := sumGroupRSSFromPS(1000, "1000 notanumber\n"); err == nil {
		t.Error("sumGroupRSSFromPS accepted a malformed rss for a matching row")
	}

	// No member present -> zero matches, so the caller can skip rather than
	// read a false 0.
	if _, matched, _ := sumGroupRSSFromPS(1000, "2000 500\n"); matched != 0 {
		t.Errorf("matched = %d, want 0 when the group is absent", matched)
	}
}

func TestSumGroupCPUFromPS(t *testing.T) {
	// pgid time, columns as `ps -Ao pgid=,time=`; time is hh:mm:ss.hh.
	out := "1000 0:01.00\n" + // 1s, in the group
		"1000 1:30.00\n" + // 90s, in the group
		"2000 5:00.00\n" // other group, ignored
	total, matched, err := sumGroupCPUFromPS(1000, out)
	if err != nil {
		t.Fatalf("sumGroupCPUFromPS: %v", err)
	}
	if matched != 2 {
		t.Errorf("matched = %d, want 2", matched)
	}
	if total != 91 {
		t.Errorf("total = %v, want 91", total)
	}

	// A malformed time for a matching row is an error, never a silent 0.
	if _, _, err := sumGroupCPUFromPS(1000, "1000 garbage\n"); err == nil {
		t.Error("sumGroupCPUFromPS accepted a malformed time for a matching row")
	}
}

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
