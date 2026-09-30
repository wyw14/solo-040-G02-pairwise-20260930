package waterway

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// ReleaseCause documents why an occupancy was safely released.
type ReleaseCause string

const (
	CauseDeparted  ReleaseCause = "departed" // field confirmed the convoy cleared the reach
	CauseCancelled ReleaseCause = "cancelled"
	CauseEmergency ReleaseCause = "emergency"
	CauseManual    ReleaseCause = "manual"
)

// ReleaseRecord is the durable payload for a safe waterway release.
type ReleaseRecord struct {
	WaterwayID WaterwayID      `json:"waterway_id"`
	ChamberID  string          `json:"chamber_id"`
	CycleID    uuid.UUID       `json:"cycle_id"`
	PlanID     uuid.UUID       `json:"plan_id"`
	Direction  model.Direction `json:"direction"`
	Convoy     string          `json:"convoy"`
	Cause      ReleaseCause    `json:"cause"`
	ReleasedAt time.Time       `json:"released_at"`
}

// Service is the durable boundary over the in-memory arbiter. Every grant or
// release is journaled under the requesting chamber partition so in-flight
// occupancy survives a restart.
type Service struct {
	arbiter  *Arbiter
	recorder *journal.Recorder
	now      func() time.Time
}

func NewService(arbiter *Arbiter, recorder *journal.Recorder, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{arbiter: arbiter, recorder: recorder, now: now}
}

// RequestClearance applies for the shared water and, when clear, journals the
// grant before the occupancy is exposed. A conflict returns a DeniedError and
// leaves the waitlist populated.
func (s *Service) RequestClearance(ctx context.Context, req Request) (Decision, error) {
	if err := req.validate(); err != nil {
		return Decision{Waterway: req.WaterwayID}, err
	}
	decision, err := s.arbiter.Grant(req, func(occupancy Occupancy) error {
		_, recordErr := s.recorder.Record(ctx, occupancy.ChamberID, model.EventWaterwayGranted,
			occupancy.CycleID, 0, occupancy)
		return recordErr
	})
	if err != nil {
		return decision, err
	}
	return decision, nil
}

// Release safely frees one convoy's hold and journals the release. It is
// idempotent: a second call reports that nothing was held.
func (s *Service) Release(ctx context.Context, waterway WaterwayID, chamberID string, cycleID, planID uuid.UUID, cause ReleaseCause) (ReleaseRecord, error) {
	if waterway == "" || chamberID == "" || cycleID == uuid.Nil {
		return ReleaseRecord{}, errors.New("release requires waterway, chamber and cycle")
	}
	if cause == "" {
		cause = CauseManual
	}
	released, ok := s.arbiter.Release(waterway, convoyKey{CycleID: cycleID, PlanID: planID})
	record := ReleaseRecord{
		WaterwayID: waterway, ChamberID: chamberID, CycleID: cycleID, PlanID: planID,
		Direction: released.Direction, Convoy: released.Convoy,
		Cause: cause, ReleasedAt: s.now().UTC(),
	}
	if !ok {
		return record, nil
	}
	_, err := s.recorder.Record(ctx, chamberID, model.EventWaterwayReleased, cycleID, 0, record)
	return record, err
}

// ReleaseChamber safely frees every hold a chamber owns (cancel/emergency).
func (s *Service) ReleaseChamber(ctx context.Context, waterway WaterwayID, chamberID string, cause ReleaseCause) ([]ReleaseRecord, error) {
	if waterway == "" || chamberID == "" {
		return nil, errors.New("release requires waterway and chamber")
	}
	released := s.arbiter.ReleaseChamber(waterway, chamberID)
	records := make([]ReleaseRecord, 0, len(released))
	var combined error
	for _, occupancy := range released {
		record := ReleaseRecord{
			WaterwayID: occupancy.WaterwayID, ChamberID: occupancy.ChamberID,
			CycleID: occupancy.CycleID, PlanID: occupancy.PlanID, Direction: occupancy.Direction,
			Convoy: occupancy.Convoy, Cause: cause, ReleasedAt: s.now().UTC(),
		}
		if _, err := s.recorder.Record(ctx, chamberID, model.EventWaterwayReleased, occupancy.CycleID, 0, record); err != nil {
			combined = errors.Join(combined, err)
		}
		records = append(records, record)
	}
	return records, combined
}

// ReleaseCycle safely frees every hold owned by a transit, journaling one
// release per occupancy. It is called when a lockage is cancelled, stopped in
// an emergency or confirmed departed. Idempotent: nothing is recorded when the
// transit held nothing.
func (s *Service) ReleaseCycle(ctx context.Context, chamberID string, cycleID uuid.UUID, cause ReleaseCause) ([]ReleaseRecord, error) {
	if chamberID == "" || cycleID == uuid.Nil {
		return nil, errors.New("cycle release requires chamber and cycle")
	}
	if cause == "" {
		cause = CauseManual
	}
	released := s.arbiter.ReleaseCycle(cycleID)
	records := make([]ReleaseRecord, 0, len(released))
	var combined error
	for _, occupancy := range released {
		record := ReleaseRecord{
			WaterwayID: occupancy.WaterwayID, ChamberID: chamberID,
			CycleID: occupancy.CycleID, PlanID: occupancy.PlanID, Direction: occupancy.Direction,
			Convoy: occupancy.Convoy, Cause: cause, ReleasedAt: s.now().UTC(),
		}
		if _, err := s.recorder.Record(ctx, chamberID, model.EventWaterwayReleased, occupancy.CycleID, 0, record); err != nil {
			combined = errors.Join(combined, err)
		}
		records = append(records, record)
	}
	return records, combined
}

// ForgetWait removes a chamber's convoy from the waitlist without granting.
func (s *Service) ForgetWait(waterway WaterwayID, cycleID uuid.UUID) {
	s.arbiter.ForgetWait(waterway, cycleID)
}

func (s *Service) Evaluate(req Request) Decision { return s.arbiter.Evaluate(req) }

func (s *Service) Views() []View { return s.arbiter.Views() }

func (s *Service) Current(waterway WaterwayID) []Occupancy { return s.arbiter.Current(waterway) }

// Restore rebuilds an occupancy from a replayed grant without journaling.
func (s *Service) Restore(occupancy Occupancy) { s.arbiter.Restore(occupancy) }
