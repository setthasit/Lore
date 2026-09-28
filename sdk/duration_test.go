package lore_test

import (
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/sdk"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr []string
	}{
		{
			name: "whole days",
			raw:  "30d",
			want: 720 * time.Hour,
		},
		{
			name: "the largest representable day count",
			raw:  "106751d",
			want: 106751 * 24 * time.Hour,
		},
		{
			name:    "one day past the largest representable count",
			raw:     "106752d",
			wantErr: []string{"at most 106751d"},
		},
		{
			name:    "a day count wider than the parser's integer",
			raw:     "9223372036854775808d",
			wantErr: []string{"at most 106751d"},
		},
		{
			name: "negative days",
			raw:  "-30d",
			want: -720 * time.Hour,
		},
		{
			name: "the smallest representable day count",
			raw:  "-106751d",
			want: -106751 * 24 * time.Hour,
		},
		{
			name:    "one day past the smallest representable count",
			raw:     "-106752d",
			wantErr: []string{"at most 106751d"},
		},
		{
			name: "stdlib form passes through",
			raw:  "90m",
			want: 90 * time.Minute,
		},
		{
			name:    "fractional days",
			raw:     "1.5d",
			wantErr: []string{"1.5d"},
		},
		{
			name:    "day suffix without a count",
			raw:     "d",
			wantErr: []string{"invalid duration"},
		},
		{
			name:    "empty",
			raw:     "",
			wantErr: []string{"invalid duration"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lore.ParseDuration(tt.raw)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("ParseDuration(%q) = %s, want an error", tt.raw, got)
				}
				if got != 0 {
					t.Errorf("ParseDuration(%q) = %s, want 0 alongside the error", tt.raw, got)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("ParseDuration(%q) error = %q, want it to mention %s", tt.raw, err, want)
					}
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
