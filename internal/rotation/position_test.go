package rotation

import (
	"testing"
	"time"
)

func TestFloorMod(t *testing.T) {
	for a := -10; a <= 10; a++ {
		for n := 1; n <= 5; n++ {
			want := ((a % n) + n) % n
			if got := floorMod(a, n); got != want {
				t.Fatalf("floorMod(%d, %d) = %d, want %d", a, n, got, want)
			}
		}
	}
}

func TestPositionAt_Daily(t *testing.T) {
	g := mustGrid(t, "UTC", dailyPolicy("11:00"))
	layer := baseSnapshot().L1 // anchor Mon 2026-08-03 11:00 UTC, 3 groups

	tests := []struct {
		name          string
		startPosition int
		at            time.Time
		wantPos       int
	}{
		{"anchor slot, position 0", 0, utc(2026, time.August, 3, 15, 0), 0},
		{"anchor slot, position 2", 2, utc(2026, time.August, 3, 15, 0), 2},
		{"two slots later", 0, utc(2026, time.August, 5, 12, 0), 2},
		{"two slots later, position 1", 1, utc(2026, time.August, 5, 12, 0), 0},
		{"wrap around", 0, utc(2026, time.August, 6, 12, 0), 0},
		{"negative elapsed", 0, utc(2026, time.August, 1, 12, 0), 1},  // -2 slots -> floorMod(-2,3)=1
		{"negative elapsed pos 2", 2, utc(2026, time.August, 1, 12, 0), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := layer.clone()
			l.StartPosition = intp(tt.startPosition)
			pos, slot, err := PositionAt(g, l, tt.at)
			if err != nil {
				t.Fatalf("PositionAt: %v", err)
			}
			if pos != tt.wantPos {
				t.Fatalf("position = %d, want %d", pos, tt.wantPos)
			}
			if tt.at.Before(slot.Start) || !tt.at.Before(slot.End) {
				t.Fatalf("returned slot [%v, %v) does not contain %v", slot.Start, slot.End, tt.at)
			}
		})
	}
}

func TestPositionAt_Weekly(t *testing.T) {
	g := mustGrid(t, "UTC", weeklyPolicy("11:00", 1)) // Monday
	layer := RotationLayerSnapshot{
		Enabled: true,
		Policy:  weeklyPolicy("11:00", 1),
		Groups: []RotationGroup{
			{ID: gid[0], Members: []string{"alice"}},
			{ID: gid[1], Members: []string{"bob"}},
		},
		PhaseAnchorSlotStart: timep(utc(2026, time.August, 3, 11, 0)), // Monday
		StartPosition:        intp(1),
	}

	tests := []struct {
		name    string
		at      time.Time
		wantPos int
	}{
		{"inside anchor week", utc(2026, time.August, 6, 0, 0), 1},
		{"next week", utc(2026, time.August, 11, 0, 0), 0},
		{"previous week", utc(2026, time.July, 30, 0, 0), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pos, _, err := PositionAt(g, layer, tt.at)
			if err != nil {
				t.Fatalf("PositionAt: %v", err)
			}
			if pos != tt.wantPos {
				t.Fatalf("position = %d, want %d", pos, tt.wantPos)
			}
		})
	}
}

func TestPositionAt_ExactHandoffBoundary(t *testing.T) {
	g := mustGrid(t, "UTC", dailyPolicy("11:00"))
	layer := baseSnapshot().L1
	boundary := utc(2026, time.August, 4, 11, 0)

	before, _, err := PositionAt(g, layer, boundary.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	atB, slot, err := PositionAt(g, layer, boundary)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := PositionAt(g, layer, boundary.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if before != 0 || atB != 1 || after != 1 {
		t.Fatalf("positions around boundary = %d/%d/%d, want 0/1/1 (half-open)", before, atB, after)
	}
	if !slot.Start.Equal(boundary) {
		t.Fatalf("at exact boundary the slot must start there, got %v", slot.Start)
	}
}

func TestPositionAt_TargetInsideOriginSlot(t *testing.T) {
	g := mustGrid(t, "UTC", dailyPolicy("11:00"))
	layer := baseSnapshot().L1
	pos, _, err := PositionAt(g, layer, utc(2026, time.August, 3, 23, 59))
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Fatalf("elapsed inside origin slot must be 0, got position %d", pos)
	}
}

func TestPositionAt_InvalidLayer(t *testing.T) {
	g := mustGrid(t, "UTC", dailyPolicy("11:00"))
	l := baseSnapshot().L1
	l.PhaseAnchorSlotStart = nil
	l.StartPosition = nil
	if _, _, err := PositionAt(g, l, utc(2026, time.August, 4, 0, 0)); err == nil {
		t.Fatalf("active layer without phase pair must be an error in PositionAt")
	}
}

func TestActiveGroupAt_DisabledOrEmpty(t *testing.T) {
	g := mustGrid(t, "UTC", dailyPolicy("11:00"))

	disabled := baseSnapshot().L1
	disabled.Enabled = false
	disabled.PhaseAnchorSlotStart = nil
	disabled.StartPosition = nil
	if _, _, ok, err := ActiveGroupAt(g, disabled, utc(2026, time.August, 4, 0, 0)); ok || err != nil {
		t.Fatalf("disabled layer: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	empty := baseSnapshot().L1
	empty.Groups = nil
	empty.PhaseAnchorSlotStart = nil
	empty.StartPosition = nil
	if _, _, ok, err := ActiveGroupAt(g, empty, utc(2026, time.August, 4, 0, 0)); ok || err != nil {
		t.Fatalf("empty layer: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	group, _, ok, err := ActiveGroupAt(g, baseSnapshot().L1, utc(2026, time.August, 4, 12, 0))
	if err != nil || !ok {
		t.Fatalf("active layer: ok=%v err=%v", ok, err)
	}
	if group.ID != gid[1] {
		t.Fatalf("active group = %s, want %s (bob)", group.ID, gid[1])
	}
	// Returned group must not alias the layer's slices.
	group.Members[0] = "mutated"
	if baseSnapshot().L1.Groups[1].Members[0] == "mutated" {
		t.Fatalf("ActiveGroupAt aliased layer members")
	}
}
