package config

import (
	"strings"
	"testing"
	"time"
)

func TestCheckIntervalDefaults(t *testing.T) {
	t.Parallel()

	var nilArchival *ArchivalCheck
	var nilSync *SyncCheck
	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{name: "archival unset", got: (&ArchivalCheck{}).Interval(), want: DefaultCheckInterval},
		{name: "archival nil", got: nilArchival.Interval(), want: DefaultCheckInterval},
		{name: "archival set", got: (&ArchivalCheck{CheckInterval: 2 * time.Minute}).Interval(), want: 2 * time.Minute},
		{name: "sync unset", got: (&SyncCheck{}).Interval(), want: DefaultCheckInterval},
		{name: "sync nil", got: nilSync.Interval(), want: DefaultCheckInterval},
		{name: "sync set", got: (&SyncCheck{CheckInterval: 10 * time.Second}).Interval(), want: 10 * time.Second},
		{name: "height unset", got: (&MultiChainNetwork{}).HeightCheckInterval(), want: DefaultCheckInterval},
		{name: "height set", got: (&MultiChainNetwork{HeightCheck: &HeightCheckConfig{Interval: 5 * time.Second}}).HeightCheckInterval(), want: 5 * time.Second},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	if DefaultCheckInterval != 30*time.Second {
		t.Errorf("DefaultCheckInterval = %v, want 30s (the former fixed cron period)", DefaultCheckInterval)
	}
}

// Intervals are validated at load: below 1s (or negative) is an error, unset
// means the default.
func TestValidateCheckIntervals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		modify  func(*MultiChainNetwork)
		wantErr string
	}{
		{name: "height 1s", modify: func(n *MultiChainNetwork) { n.HeightCheck.Interval = time.Second }},
		{name: "height unset", modify: func(n *MultiChainNetwork) { n.HeightCheck.Interval = 0 }},
		{name: "height below 1s", modify: func(n *MultiChainNetwork) { n.HeightCheck.Interval = 500 * time.Millisecond }, wantErr: "height_check interval too short"},
		{name: "height negative", modify: func(n *MultiChainNetwork) { n.HeightCheck.Interval = -time.Second }, wantErr: "height_check interval too short"},
		{name: "archival 10s", modify: func(n *MultiChainNetwork) { n.Archival = &ArchivalCheck{MinHeight: 1, CheckInterval: 10 * time.Second} }},
		{name: "archival below 1s", modify: func(n *MultiChainNetwork) { n.Archival = &ArchivalCheck{MinHeight: 1, CheckInterval: time.Millisecond} }, wantErr: "archival check_interval too short"},
		{name: "sync 5s", modify: func(n *MultiChainNetwork) {
			n.Sync = &SyncCheck{MaxDrift: 5, CheckInterval: 5 * time.Second, Oracles: []SyncOracle{{URL: "https://oracle.example.com"}}}
		}},
		{name: "sync negative", modify: func(n *MultiChainNetwork) {
			n.Sync = &SyncCheck{MaxDrift: 5, CheckInterval: -time.Second, Oracles: []SyncOracle{{URL: "https://oracle.example.com"}}}
		}, wantErr: "sync check_interval too short"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validMultiChainConfig()
			tt.modify(&cfg.Networks[0])
			err := ValidateMultiChain(&cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "pocket") {
				t.Fatalf("err = %v, want it to contain %q and the network name", err, tt.wantErr)
			}
		})
	}
}
