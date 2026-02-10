package models

import (
	"testing"
	"time"
)

func TestAccountAgeTierFromAge(t *testing.T) {
	now := time.Date(2026, 2, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		createdAt time.Time
		want      AccountAgeTier
	}{
		{"brand new (1 day)", now.AddDate(0, 0, -1), TierNew},
		{"89 days", now.AddDate(0, 0, -89), TierNew},
		{"exactly 90 days", now.Add(-90 * 24 * time.Hour), TierEstablished},
		{"91 days", now.AddDate(0, 0, -91), TierEstablished},
		{"1 year", now.AddDate(-1, 0, 0), TierEstablished},
		{"just under 2 years", now.Add(-2*365*24*time.Hour + time.Hour), TierEstablished},
		{"exactly 2 years", now.Add(-2 * 365 * 24 * time.Hour), TierVeteran},
		{"3 years", now.AddDate(-3, 0, 0), TierVeteran},
		{"10 years", now.AddDate(-10, 0, 0), TierVeteran},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AccountAgeTierFromAge(tt.createdAt, now)
			if got != tt.want {
				t.Errorf("AccountAgeTierFromAge() = %q, want %q", got, tt.want)
			}
		})
	}
}
