//go:build integration

package schedulerender

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
)

// benchScheduleCount is the worst case the tick is measured against: a hundred
// ACTIVE schedules, because 1 + 2A is the worst case and the average over some
// installation is not what has to hold.
const benchScheduleCount = 100

// BenchmarkCurrentOnCallForAll measures one notifier tick against PostgreSQL.
//
// What it measures is round trips, not rotation math. The per-schedule reads are
// sequential, they run on one connection inside one snapshot, and they set both
// the duration of a tick and how long that connection is held - so ns/op here is
// also the connection-hold time per tick. The 0.35 ms PositionAt figure from
// Sprint 3 says nothing about any of that.
//
// The anchor is a year old on purpose: that is where the phase anchor of a
// schedule nobody has edited ends up, and the anchor walk is the one part of the
// projection that grows with age.
//
//	TEST_DB_DSN=... go test -tags integration ./internal/schedulerender/ \
//	  -run '^$' -bench BenchmarkCurrentOnCallForAll -benchtime 10x
//
// The result belongs in the "Измерения" section of the sprint document, with the
// hardware and the date. A number nobody wrote down is a number nobody can
// compare against.
func BenchmarkCurrentOnCallForAll(b *testing.B) {
	s := testutil.SetupDB(b)

	// A year-old anchor, and "now" for the projection a year later.
	anchor := time.Date(2025, 5, 5, 11, 0, 0, 0, time.UTC)
	now := anchor.AddDate(1, 0, 0).Add(7 * time.Hour)

	config := scheduleconfig.NewService(s.ScheduleConfigRepository(),
		scheduleconfig.WithClock(func() time.Time { return anchor }))

	for i := 0; i < benchScheduleCount; i++ {
		seedBenchSchedule(b, s, config, i)
	}

	svc := New(s.ScheduleReadRepository(), WithClock(func() time.Time { return now }))
	ctx := context.Background()

	// One warm call, so the benchmark measures steady state rather than the
	// first connection handshake.
	warm, err := svc.CurrentOnCallForAllNow(ctx)
	if err != nil {
		b.Fatalf("CurrentOnCallForAllNow: %v", err)
	}
	if len(warm.Schedules) != benchScheduleCount || len(warm.Failures) != 0 {
		b.Fatalf("fixture projects %d schedules with %d failures, want %d and 0",
			len(warm.Schedules), len(warm.Failures), benchScheduleCount)
	}
	onDuty := 0
	for _, sc := range warm.Schedules {
		if sc.OnCall.L1 != nil {
			onDuty++
		}
	}
	if onDuty != benchScheduleCount {
		b.Fatalf("%d of %d schedules have somebody on duty; the worst case needs all of them",
			onDuty, benchScheduleCount)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.CurrentOnCallForAllNow(ctx); err != nil {
			b.Fatalf("CurrentOnCallForAllNow: %v", err)
		}
	}
}

// seedBenchSchedule creates one team, its two members and its schedule through
// the real command service, so the fixture is a schedule the product could have
// produced rather than rows assembled by hand.
func seedBenchSchedule(b *testing.B, s *store.Store, config *scheduleconfig.Service, i int) {
	b.Helper()
	teamID := fmt.Sprintf("bench-team-%03d", i)
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: teamID}); err != nil {
		b.Fatalf("CreateTeam %s: %v", teamID, err)
	}
	members := []string{teamID + "-a", teamID + "-b"}
	for _, id := range members {
		if err := s.CreateUser(&model.User{ID: id, Name: id, Email: id + "@bench.test"}); err != nil {
			b.Fatalf("CreateUser %s: %v", id, err)
		}
		if err := s.AddTeamMember(teamID, id, model.TeamMemberRoleMember); err != nil {
			b.Fatalf("AddTeamMember %s: %v", id, err)
		}
	}

	policy := rotation.RotationPolicy{
		Cadence:       model.RotationDaily,
		HandoffTime:   "11:00",
	}
	cfg := rotation.ScheduleConfiguration{
		Timezone: "Europe/Berlin",
		L1: rotation.LayerConfiguration{
			Enabled: true,
			Policy:  policy,
			Groups: []rotation.RotationGroup{
				{ID: benchGroupID(i, 1), Members: members[:1]},
				{ID: benchGroupID(i, 2), Members: members[1:]},
			},
		},
		L2:                      rotation.LayerConfiguration{Enabled: false, Policy: policy},
		L2EscalationTimeoutMins: 5,
	}
	if _, err := config.Save(context.Background(), teamID, scheduleconfig.SaveCommand{
		Desired: cfg,
	}); err != nil {
		b.Fatalf("create schedule %s: %v", teamID, err)
	}
}

// benchGroupID builds a canonical UUID per (schedule, group), which is what an
// L1 group identity has to be.
func benchGroupID(schedule, group int) string {
	return fmt.Sprintf("%08d-4444-4444-8444-%012d", schedule, group)
}
