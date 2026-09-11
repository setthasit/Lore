package lore_test

import (
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{
			name: "whole days",
			raw:  "30d",
			want: 720 * time.Hour,
		},
		{
			name: "stdlib form passes through",
			raw:  "90m",
			want: 90 * time.Minute,
		},
		{
			name:    "fractional days",
			raw:     "1.5d",
			wantErr: true,
		},
		{
			name:    "day suffix without a count",
			raw:     "d",
			wantErr: true,
		},
		{
			name:    "empty",
			raw:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lore.ParseDuration(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseDuration(%q) = %s, want an error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("ParseDuration(%q) = %s, want %s", tt.raw, got, tt.want)
			}
		})
	}
}
