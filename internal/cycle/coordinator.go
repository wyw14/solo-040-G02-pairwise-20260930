package cycle

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/hydraulics"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/waterway"
)

// WaterwayReleaser safely frees shared-water occupancy when a transit ends.
type WaterwayReleaser interface {
	ReleaseCycle(ctx context.Context, chamberID string, cycleID uuid.UUID, cause waterway.ReleaseCause) ([]waterway.ReleaseRecord, error)
}

// Coordinator is the authoritative owner of active transit generations.
type Coordinator struct {
	mu          sync.RWMutex
	current     map[string]model.TransitCycle
	generations map[string]uint64
	gates       *gate.Controller
	hydraulics  *hydraulics.Controller
	recorder    *journal.Recorder
	waterway    WaterwayReleaser
	now         func() time.Time
}

func NewCoordinator(gates *gate.Controller, hydraulicsController *hydraulics.Controller, recorder *journal.Recorder, now func() time.Time) *Coordinator {
	if now == nil {
		now = time.Now
	}
	return &Coordinator{
		current: make(map[string]model.TransitCycle), generations: make(map[string]uint64),
		gates: gates, hydraulics: hydraulicsController, recorder: recorder, now: now,
	}
}

// BindWaterwayReleaser wires the shared-water arbiter so terminal transitions
// safely release occupancy. It mirrors the late-binding used by signal handoff.
func (c *Coordinator) BindWaterwayReleaser(releaser WaterwayReleaser) { c.waterway = releaser }

// releaseWaterway is best-effort after the terminal event is durable; a
// release failure must not hide the recorded transition.
func (c *Coordinator) releaseWaterway(ctx context.Context, chamberID string, cycleValue model.TransitCycle, cause waterway.ReleaseCause) error {
	if c.waterway == nil {
		return nil
	}
	_, err := c.waterway.ReleaseCycle(ctx, chamberID, cycleValue.ID, cause)
	return err
}

func (c *Coordinator) Start(ctx context.Context, plan model.ConvoyPlan) (model.TransitCycle, error) {
	c.mu.Lock()
	if active, ok := c.current[plan.ChamberID]; ok && !active.Stage.Terminal() {
		c.mu.Unlock()
		return model.TransitCycle{}, errors.New("chamber already has an active transit")
	}
	c.generations[plan.ChamberID]++
	cycleValue, err := model.NewTransitCycle(plan.ChamberID, plan.ID, plan.Direction, c.generations[plan.ChamberID], c.now())
	if err == nil {
		c.current[plan.ChamberID] = cycleValue
	}
	c.mu.Unlock()
	if err != nil {
		return model.TransitCycle{}, err
	}
	if _, err := c.recorder.Record(ctx, plan.ChamberID, model.EventCycleCreated, cycleValue.ID, cycleValue.Generation, cycleValue); err != nil {
		return model.TransitCycle{}, err
	}
	return cycleValue, nil
}

func (c *Coordinator) Current(chamberID string) (model.TransitCycle, bool) {
	c.mu.RLock()
	cycleValue, ok := c.current[chamberID]
	c.mu.RUnlock()
	return cycleValue.Clone(), ok
}

func (c *Coordinator) Cancel(ctx context.Context, chamberID, reason string) (model.TransitCycle, error) {
	c.mu.Lock()
	cycleValue, ok := c.current[chamberID]
	if !ok || cycleValue.Stage.Terminal() {
		c.mu.Unlock()
		return model.TransitCycle{}, errors.New("no cancellable transit")
	}
	if err := cycleValue.Advance(terminalStage(reason), reason, c.now()); err != nil {
		c.mu.Unlock()
		return model.TransitCycle{}, err
	}
	c.current[chamberID] = cycleValue
	c.mu.Unlock()
	if _, err := c.recorder.Record(ctx, chamberID, model.EventCycleAdvanced, cycleValue.ID, cycleValue.Generation, cycleValue); err != nil {
		return cycleValue, err
	}
	cause := waterway.CauseCancelled
	if terminalStage(reason) == model.StageEmergency {
		cause = waterway.CauseEmergency
	}
	if err := c.releaseWaterway(ctx, chamberID, cycleValue, cause); err != nil {
		return cycleValue, err
	}
	return cycleValue, nil
}

func (c *Coordinator) ApplyGateAck(ctx context.Context, ack model.DeviceAck) error {
	command, err := c.gates.Confirm(ctx, ack)
	if err != nil {
		return err
	}
	c.mu.Lock()
	cycleValue, ok := c.current[ack.ChamberID]
	if !ok || cycleValue.Stage.Terminal() || !cycleValue.IdentityMatches(command.CycleID, command.Generation) {
		c.mu.Unlock()
		return errors.New("device acknowledgement belongs to an inactive transit generation")
	}
	next, err := nextAfterGate(cycleValue.Stage)
	if err == nil {
		err = cycleValue.Advance(next, "matching gate command settled", c.now())
	}
	if err == nil {
		c.current[ack.ChamberID] = cycleValue
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if next == model.StageLeveling {
		if err := c.hydraulics.BeginLeveling(cycleValue); err != nil {
			return err
		}
	}
	_, err = c.recorder.Record(ctx, ack.ChamberID, model.EventCycleAdvanced, cycleValue.ID, cycleValue.Generation, cycleValue)
	return err
}

func (c *Coordinator) ApplySignalHandoff(ctx context.Context, cycleID uuid.UUID, generation uint64) error {
	c.mu.Lock()
	var chamberID string
	var cycleValue model.TransitCycle
	for id, candidate := range c.current {
		if candidate.IdentityMatches(cycleID, generation) {
			chamberID, cycleValue = id, candidate
			break
		}
	}
	if chamberID == "" {
		c.mu.Unlock()
		return errors.New("signal handoff transit is not current")
	}
	if err := applySignalTransition(&cycleValue); err != nil {
		c.mu.Unlock()
		return err
	}
	cycleValue.UpdatedAt = c.now().UTC()
	completed := cycleValue.Stage == model.StageCompleted
	c.current[chamberID] = cycleValue
	c.mu.Unlock()
	if _, err := c.recorder.Record(ctx, chamberID, model.EventCycleAdvanced, cycleValue.ID, generation, cycleValue); err != nil {
		return err
	}
	if completed {
		if err := c.releaseWaterway(ctx, chamberID, cycleValue, waterway.CauseDeparted); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) Restore(cycleValue model.TransitCycle) {
	c.mu.Lock()
	c.current[cycleValue.ChamberID] = cycleValue
	if c.generations[cycleValue.ChamberID] < cycleValue.Generation {
		c.generations[cycleValue.ChamberID] = cycleValue.Generation
	}
	c.mu.Unlock()
}
