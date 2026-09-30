package cycle

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/hydraulics"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/waterway"
)

type recordedRelease struct {
	chamber string
	cycleID uuid.UUID
	cause   waterway.ReleaseCause
}

type fakeWaterwayReleaser struct{ calls []recordedRelease }

func (f *fakeWaterwayReleaser) ReleaseCycle(_ context.Context, chamberID string, cycleID uuid.UUID, cause waterway.ReleaseCause) ([]waterway.ReleaseRecord, error) {
	f.calls = append(f.calls, recordedRelease{chamber: chamberID, cycleID: cycleID, cause: cause})
	return []waterway.ReleaseRecord{{ChamberID: chamberID, CycleID: cycleID, Cause: cause}}, nil
}

func newTestCoordinator(releaser WaterwayReleaser) *Coordinator {
	now := func() time.Time { return time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC) }
	store := journal.NewMemoryStore()
	recorder := journal.NewRecorder(store, now)
	gateController := gate.NewController(gate.NewState(), gate.NewCommands(), recorder, now)
	ramp := hydraulics.NewRamp(time.Millisecond, now)
	hydraulicsController := hydraulics.NewController(gateController.Commands(), recorder, ramp, now)
	coordinator := NewCoordinator(gateController, hydraulicsController, recorder, now)
	coordinator.BindWaterwayReleaser(releaser)
	return coordinator
}

func startCycle(t *testing.T, coordinator *Coordinator, chamber string) model.TransitCycle {
	t.Helper()
	plan, err := model.NewConvoyPlan(chamber, model.DirectionUpstream,
		[]model.Vessel{{ID: uuid.New(), Name: "River Star", CallSign: "RS-1", LengthM: 80}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	cycleValue, err := coordinator.Start(context.Background(), plan)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return cycleValue
}

func TestCancelReleasesWaterwayAsCancelled(t *testing.T) {
	releaser := &fakeWaterwayReleaser{}
	coordinator := newTestCoordinator(releaser)
	cycleValue := startCycle(t, coordinator, "chamber-1")

	if _, err := coordinator.Cancel(context.Background(), "chamber-1", "operator cancellation"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(releaser.calls) != 1 || releaser.calls[0].cause != waterway.CauseCancelled {
		t.Fatalf("expected one cancelled release, got %+v", releaser.calls)
	}
	if releaser.calls[0].cycleID != cycleValue.ID {
		t.Fatalf("released wrong cycle: %+v", releaser.calls[0])
	}
}

func TestEmergencyReleasesWaterwayAsEmergency(t *testing.T) {
	releaser := &fakeWaterwayReleaser{}
	coordinator := newTestCoordinator(releaser)
	startCycle(t, coordinator, "chamber-2")

	if _, err := coordinator.Cancel(context.Background(), "chamber-2", "emergency"); err != nil {
		t.Fatalf("emergency cancel: %v", err)
	}
	if len(releaser.calls) != 1 || releaser.calls[0].cause != waterway.CauseEmergency {
		t.Fatalf("expected one emergency release, got %+v", releaser.calls)
	}
}

func TestConfirmedDepartureReleasesWaterwayAsDeparted(t *testing.T) {
	releaser := &fakeWaterwayReleaser{}
	coordinator := newTestCoordinator(releaser)
	cycleValue := startCycle(t, coordinator, "chamber-1")

	// Place the transit in the releasing stage, as field gate acks would.
	cycleValue.Stage = model.StageReleasing
	coordinator.Restore(cycleValue)

	if err := coordinator.ApplySignalHandoff(context.Background(), cycleValue.ID, cycleValue.Generation); err != nil {
		t.Fatalf("signal handoff: %v", err)
	}
	if len(releaser.calls) != 1 || releaser.calls[0].cause != waterway.CauseDeparted {
		t.Fatalf("expected one departed release, got %+v", releaser.calls)
	}
}
