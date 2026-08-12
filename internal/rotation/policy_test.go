package rotation

import (
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

func TestParseHandoffTime(t *testing.T) {
	tests := []struct {
		in      string
		hh, mm  int
		wantErr bool
	}{
		{in: "00:00", hh: 0, mm: 0},
		{in: "09:05", hh: 9, mm: 5},
		{in: "11:00", hh: 11, mm: 0},
		{in: "23:59", hh: 23, mm: 59},
		{in: "24:00", wantErr: true},
		{in: "25:00", wantErr: true},
		{in: "11:60", wantErr: true},
		{in: "9:00", wantErr: true},  // missing leading zero is not canonical
		{in: "09:5", wantErr: true},
		{in: "011:00", wantErr: true},
		{in: "11.00", wantErr: true},
		{in: "aa:bb", wantErr: true},
		{in: "", wantErr: true},
		{in: "11:0a", wantErr: true},
		{in: "-1:00", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			hh, mm, err := ParseHandoffTime(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseHandoffTime(%q): expected error, got %d:%d", tt.in, hh, mm)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHandoffTime(%q): %v", tt.in, err)
			}
			if hh != tt.hh || mm != tt.mm {
				t.Fatalf("ParseHandoffTime(%q) = %d:%d, want %d:%d", tt.in, hh, mm, tt.hh, tt.mm)
			}
		})
	}
}

func TestRotationPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		p       RotationPolicy
		wantErr bool
	}{
		{name: "daily ok", p: dailyPolicy("11:00")},
		{name: "weekly ok", p: weeklyPolicy("11:00", 0)},
		{name: "weekly saturday", p: weeklyPolicy("23:59", 6)},
		{name: "daily with handoff day", p: RotationPolicy{Cadence: model.RotationDaily, HandoffTime: "11:00", HandoffDay: intp(1)}, wantErr: true},
		{name: "weekly without day", p: RotationPolicy{Cadence: model.RotationWeekly, HandoffTime: "11:00"}, wantErr: true},
		{name: "weekly day too big", p: weeklyPolicy("11:00", 7), wantErr: true},
		{name: "weekly day negative", p: weeklyPolicy("11:00", -1), wantErr: true},
		{name: "bad handoff time", p: dailyPolicy("9:00"), wantErr: true},
		{name: "unknown cadence", p: RotationPolicy{Cadence: "monthly", HandoffTime: "11:00"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.p.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
