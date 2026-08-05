package containment

import (
	"testing"
	"time"
)

func TestLimits_Effectives(t *testing.T) {
	var zero Limits
	if got := zero.EffectivePollInterval(); got != DefaultPollInterval {
		t.Errorf("poll interval = %v, want %v", got, DefaultPollInterval)
	}
	if got := zero.EffectiveGracefulTimeout(); got != DefaultGracefulTimeout {
		t.Errorf("graceful timeout = %v, want %v", got, DefaultGracefulTimeout)
	}

	set := Limits{PollInterval: 10 * time.Millisecond, GracefulTimeout: time.Second}
	if got := set.EffectivePollInterval(); got != 10*time.Millisecond {
		t.Errorf("poll interval = %v, want 10ms", got)
	}
	if got := set.EffectiveGracefulTimeout(); got != time.Second {
		t.Errorf("graceful timeout = %v, want 1s", got)
	}
}

func TestLimits_Bounded(t *testing.T) {
	cases := []struct {
		name string
		l    Limits
		want bool
	}{
		{"zero", Limits{}, false},
		{"rss", Limits{MaxRSSBytes: 1}, true},
		{"cpu", Limits{MaxCPUSeconds: 0.5}, true},
		// MaxProcesses is kernel-programmed, not polled, so it does not on
		// its own justify starting a sampler.
		{"processes only", Limits{MaxProcesses: 4}, false},
		{"poll interval only", Limits{PollInterval: time.Second}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.l.Bounded(); got != tc.want {
				t.Errorf("Bounded() = %v, want %v", got, tc.want)
			}
		})
	}
}
