package waterway

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

func fixedClock() func() time.Time {
	base := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	calls := 0
	return func() time.Time {
		calls++
		return base.Add(time.Duration(calls) * time.Second)
	}
}

func testVessel(name string) model.Vessel {
	return model.Vessel{ID: uuid.New(), Name: name, CallSign: "CS-" + name, LengthM: 80}
}

func newRequest(chamber string, direction model.Direction, vessel model.Vessel) Request {
	return Request{
		WaterwayID: SharedReach, ChamberID: chamber,
		CycleID: uuid.New(), PlanID: uuid.New(), Direction: direction,
		Convoy: vessel.Name, Vessels: []model.Vessel{vessel},
	}
}

func grant(t *testing.T, service *Service, req Request) Occupancy {
	t.Helper()
	decision, err := service.RequestClearance(context.Background(), req)
	if err != nil {
		t.Fatalf("expected clearance for %s heading %s: %v", req.ChamberID, req.Direction, err)
	}
	if !decision.Allowed || decision.Occupancy == nil {
		t.Fatalf("expected allowed decision with occupancy, got %+v", decision)
	}
	return *decision.Occupancy
}

func TestOpposedHeadingsCannotBothBeCleared(t *testing.T) {
	service := NewService(NewArbiter(fixedClock()), journal.NewRecorder(journal.NewMemoryStore(), fixedClock()), fixedClock())

	upstream := newRequest("chamber-1", model.DirectionUpstream, testVessel("River Star"))
	downstream := newRequest("chamber-2", model.DirectionDownstream, testVessel("Sea Breeze"))

	grant(t, service, upstream)
	_, err := service.RequestClearance(context.Background(), downstream)
	if err == nil {
		t.Fatal("expected conflicting heading to be refused")
	}
	denied, ok := err.(*DeniedError)
	if !ok {
		t.Fatalf("expected *DeniedError, got %T: %v", err, err)
	}
	if denied.Denial.Code != ReasonOpposedHeading {
		t.Fatalf("expected opposed_heading reason, got %q", denied.Denial.Code)
	}
	if denied.Denial.BlockingChamber != "chamber-1" || denied.Denial.BlockingConvoy != "River Star" {
		t.Fatalf("denial does not identify blocker: %+v", denied.Denial)
	}

	views := service.Views()
	if len(views) != 1 || len(views[0].Waitlist) != 1 {
		t.Fatalf("expected one waiting convoy, got %+v", views)
	}
	if views[0].Occupants[0].Convoy != "River Star" {
		t.Fatalf("wrong occupant after refusal: %+v", views[0].Occupants)
	}
}

func TestSameHeadingConvoysShareTheReach(t *testing.T) {
	service := NewService(NewArbiter(fixedClock()), journal.NewRecorder(journal.NewMemoryStore(), fixedClock()), fixedClock())

	first := newRequest("chamber-1", model.DirectionUpstream, testVessel("River Star"))
	second := newRequest("chamber-2", model.DirectionUpstream, testVessel("Sea Breeze"))

	grant(t, service, first)
	grant(t, service, second)

	occupants := service.Current(SharedReach)
	if len(occupants) != 2 {
		t.Fatalf("expected two same-heading occupants, got %d", len(occupants))
	}
	if service.Views()[0].Waitlist != nil && len(service.Views()[0].Waitlist) != 0 {
		t.Fatal("same-heading request must not enter the waitlist")
	}
}

func TestReRequestBySameConvoyIsIdempotent(t *testing.T) {
	service := NewService(NewArbiter(fixedClock()), journal.NewRecorder(journal.NewMemoryStore(), fixedClock()), fixedClock())
	req := newRequest("chamber-1", model.DirectionUpstream, testVessel("River Star"))

	grant(t, service, req)
	again := grant(t, service, req)
	if again.GrantedAt.IsZero() {
		t.Fatal("re-request should return the existing occupancy")
	}
	if len(service.Current(SharedReach)) != 1 {
		t.Fatal("re-request must not create a second occupancy")
	}
}

func TestReleaseByCycleClearsOccupantAndWaitlist(t *testing.T) {
	service := NewService(NewArbiter(fixedClock()), journal.NewRecorder(journal.NewMemoryStore(), fixedClock()), fixedClock())
	upstream := newRequest("chamber-1", model.DirectionUpstream, testVessel("River Star"))
	downstream := newRequest("chamber-2", model.DirectionDownstream, testVessel("Sea Breeze"))

	grant(t, service, upstream)
	if _, err := service.RequestClearance(context.Background(), downstream); err == nil {
		t.Fatal("expected conflict")
	}

	records, err := service.ReleaseCycle(context.Background(), upstream.ChamberID, upstream.CycleID, CauseDeparted)
	if err != nil {
		t.Fatalf("release failed: %v", err)
	}
	if len(records) != 1 || records[0].Cause != CauseDeparted {
		t.Fatalf("expected one departed release record, got %+v", records)
	}

	// The previously blocked convoy can now be cleared.
	grant(t, service, downstream)
	views := service.Views()
	if len(views) != 1 || len(views[0].Occupants) != 1 || len(views[0].Waitlist) != 0 {
		t.Fatalf("after release the waiter should be grantable, got %+v", views)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	service := NewService(NewArbiter(fixedClock()), journal.NewRecorder(journal.NewMemoryStore(), fixedClock()), fixedClock())
	req := newRequest("chamber-1", model.DirectionUpstream, testVessel("River Star"))
	grant(t, service, req)

	if _, err := service.ReleaseCycle(context.Background(), req.ChamberID, req.CycleID, CauseManual); err != nil {
		t.Fatal(err)
	}
	records, err := service.ReleaseCycle(context.Background(), req.ChamberID, req.CycleID, CauseManual)
	if err != nil || len(records) != 0 {
		t.Fatalf("second release should record nothing, got %d records, err=%v", len(records), err)
	}
}

func TestRecoveryRestoresInFlightOccupancyAcrossRestart(t *testing.T) {
	store := journal.NewMemoryStore()
	clock := fixedClock()
	service := NewService(NewArbiter(clock), journal.NewRecorder(store, clock), clock)

	held := newRequest("chamber-1", model.DirectionUpstream, testVessel("River Star"))
	cleared := newRequest("chamber-2", model.DirectionUpstream, testVessel("Sea Breeze"))
	grant(t, service, held)
	grant(t, service, cleared)
	if _, err := service.ReleaseCycle(context.Background(), cleared.ChamberID, cleared.CycleID, CauseDeparted); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: a fresh arbiter/service folds the same journal.
	rebuilt := NewService(NewArbiter(clock), journal.NewRecorder(store, clock), clock)
	recovery := NewRecovery(store, rebuilt)
	restored, err := recovery.RestoreAll(context.Background())
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("expected only the in-flight occupancy restored, got %d: %+v", len(restored), restored)
	}
	if restored[0].CycleID != held.CycleID || restored[0].Direction != model.DirectionUpstream {
		t.Fatalf("wrong occupancy restored: %+v", restored[0])
	}

	// The restored hold still arbitrates conflicts.
	conflict := newRequest("chamber-2", model.DirectionDownstream, testVessel("Late Comer"))
	if _, err := rebuilt.RequestClearance(context.Background(), conflict); err == nil {
		t.Fatal("restored occupancy must still block opposed headings")
	}
}
