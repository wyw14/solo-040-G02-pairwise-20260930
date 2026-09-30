package waterway

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

// Arbiter serialises directional access to shared pilotage water. Convoys on
// opposed headings can never be cleared at the same time; convoys sharing a
// heading may occupy the segment together (one following the other). Rejected
// requests are remembered so the operations page can show who waits and why.
type Arbiter struct {
	mu      sync.Mutex
	active  map[WaterwayID][]Occupancy
	waiting map[WaterwayID][]Denial
	seen    map[WaterwayID]map[convoyKey]struct{}
	now     func() time.Time
}

func NewArbiter(now func() time.Time) *Arbiter {
	if now == nil {
		now = time.Now
	}
	return &Arbiter{
		active:  make(map[WaterwayID][]Occupancy),
		waiting: make(map[WaterwayID][]Denial),
		seen:    make(map[WaterwayID]map[convoyKey]struct{}),
		now:     now,
	}
}

// Evaluate decides a request without changing occupancy and refreshes the
// waitlist so block reasons stay current between requests.
func (a *Arbiter) Evaluate(req Request) Decision {
	if err := req.validate(); err != nil {
		return Decision{Waterway: req.WaterwayID, Denial: a.invalid(req, err)}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.decideLocked(req)
}

// Grant atomically evaluates the request and runs commit before the occupancy
// becomes visible; a failed commit leaves the waterway untouched.
func (a *Arbiter) Grant(req Request, commit func(Occupancy) error) (Decision, error) {
	if err := req.validate(); err != nil {
		return Decision{Waterway: req.WaterwayID}, err
	}
	if commit == nil {
		return Decision{Waterway: req.WaterwayID}, errWaterway("grant requires a commit callback")
	}
	a.mu.Lock()
	decision := a.decideLocked(req)
	if !decision.Allowed {
		a.mu.Unlock()
		return decision, &DeniedError{Denial: *decision.Denial}
	}
	occupancy := *decision.Occupancy
	if err := commit(occupancy); err != nil {
		a.mu.Unlock()
		return Decision{Waterway: req.WaterwayID}, err
	}
	a.upsertLocked(occupancy)
	a.forgetCycleLocked(req.WaterwayID, req.CycleID)
	a.mu.Unlock()
	return Decision{Allowed: true, Waterway: req.WaterwayID, Occupancy: &occupancy}, nil
}

func (a *Arbiter) decideLocked(req Request) Decision {
	occupants := a.active[req.WaterwayID]
	for index := range occupants {
		occupant := &occupants[index]
		if occupant.key() == req.key() {
			// Re-request by the convoy already cleared: still cleared, idempotent.
			current := occupant.clone()
			return Decision{Allowed: true, Waterway: req.WaterwayID, Occupancy: &current}
		}
		if occupant.Direction != req.Direction {
			return a.denyLocked(req, *occupant,
				fmt.Sprintf("conflicting heading: convoy %q from %s heads %s through %s; request from %s heads %s",
					occupant.Convoy, occupant.ChamberID, occupant.Direction, req.WaterwayID, req.ChamberID, req.Direction))
		}
	}
	occupancy := Occupancy{
		WaterwayID: req.WaterwayID, ChamberID: req.ChamberID, CycleID: req.CycleID,
		PlanID: req.PlanID, Direction: req.Direction, Convoy: req.Convoy,
		Vessels:   append([]model.Vessel(nil), req.Vessels...),
		GrantedAt: a.now().UTC(), UpdatedAt: a.now().UTC(),
	}
	return Decision{Allowed: true, Waterway: req.WaterwayID, Occupancy: &occupancy}
}

func (a *Arbiter) denyLocked(req Request, occupant Occupancy, reason string) Decision {
	denial := Denial{
		Code: ReasonOpposedHeading, Reason: reason, WaterwayID: req.WaterwayID,
		RequestedBy: req.ChamberID, RequestedCycle: req.CycleID, RequestedPlan: req.PlanID,
		Direction:       req.Direction,
		BlockingChamber: occupant.ChamberID, BlockingCycle: occupant.CycleID,
		BlockingConvoy: occupant.Convoy, BlockingHeading: occupant.Direction,
		ObservedAt: a.now().UTC(),
	}
	if a.seen[req.WaterwayID] == nil {
		a.seen[req.WaterwayID] = make(map[convoyKey]struct{})
	}
	if _, known := a.seen[req.WaterwayID][req.key()]; !known {
		a.seen[req.WaterwayID][req.key()] = struct{}{}
		a.waiting[req.WaterwayID] = append(a.waiting[req.WaterwayID], denial)
	} else {
		wait := a.waiting[req.WaterwayID]
		for index := range wait {
			if wait[index].RequestedCycle == req.CycleID {
				wait[index] = denial
				break
			}
		}
	}
	return Decision{Allowed: false, Waterway: req.WaterwayID, Denial: &denial}
}

func (a *Arbiter) upsertLocked(occupancy Occupancy) {
	occupants := a.active[occupancy.WaterwayID]
	for index := range occupants {
		if occupants[index].key() == occupancy.key() {
			occupants[index] = occupancy
			a.active[occupancy.WaterwayID] = occupants
			return
		}
	}
	a.active[occupancy.WaterwayID] = append(occupants, occupancy)
}

// Release frees one convoy's hold. Only the occupying convoy can release it;
// the boolean reports whether a hold actually changed.
func (a *Arbiter) Release(waterway WaterwayID, key convoyKey) (Occupancy, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	occupants := a.active[waterway]
	for index := range occupants {
		if occupants[index].key() == key {
			released := occupants[index]
			a.active[waterway] = append(append([]Occupancy(nil), occupants[:index]...), occupants[index+1:]...)
			if len(a.active[waterway]) == 0 {
				delete(a.active, waterway)
				a.clearWaitLocked(waterway)
			}
			a.forgetCycleLocked(waterway, key.CycleID)
			return released, true
		}
	}
	return Occupancy{}, false
}

// ReleaseChamber releases every hold a chamber holds on a waterway (used by
// cancellation and emergency safe-release).
func (a *Arbiter) ReleaseChamber(waterway WaterwayID, chamberID string) []Occupancy {
	a.mu.Lock()
	defer a.mu.Unlock()
	var released []Occupancy
	kept := make([]Occupancy, 0, len(a.active[waterway]))
	for _, occupant := range a.active[waterway] {
		if occupant.ChamberID == chamberID {
			released = append(released, occupant)
			a.forgetCycleLocked(waterway, occupant.CycleID)
			continue
		}
		kept = append(kept, occupant)
	}
	if len(kept) == 0 {
		delete(a.active, waterway)
		a.clearWaitLocked(waterway)
	} else {
		a.active[waterway] = kept
	}
	return released
}

func (a *Arbiter) forgetCycleLocked(waterway WaterwayID, cycleID uuid.UUID) {
	wait := a.waiting[waterway]
	kept := wait[:0]
	for _, denial := range wait {
		if denial.RequestedCycle == cycleID {
			delete(a.seen[waterway], convoyKey{CycleID: cycleID, PlanID: denial.RequestedPlan})
			continue
		}
		kept = append(kept, denial)
	}
	a.waiting[waterway] = append([]Denial(nil), kept...)
}

// clearWaitLocked drops all block reasons once a waterway is free: with no
// occupant there is nothing left to block convoys, so waiters re-apply.
func (a *Arbiter) clearWaitLocked(waterway WaterwayID) {
	delete(a.waiting, waterway)
	delete(a.seen, waterway)
}

// ForgetWait drops a convoy that no longer wants the segment (cancel/leave).
func (a *Arbiter) ForgetWait(waterway WaterwayID, cycleID uuid.UUID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forgetCycleLocked(waterway, cycleID)
}

// ReleaseCycle releases every hold owned by a transit and removes it from all
// waitlists. It is the single hook used when a lockage ends (cancel, emergency
// or confirmed departure).
func (a *Arbiter) ReleaseCycle(cycleID uuid.UUID) []Occupancy {
	a.mu.Lock()
	defer a.mu.Unlock()
	var released []Occupancy
	for waterway, occupants := range a.active {
		kept := make([]Occupancy, 0, len(occupants))
		for _, occupant := range occupants {
			if occupant.CycleID == cycleID {
				released = append(released, occupant)
				continue
			}
			kept = append(kept, occupant)
		}
		if len(kept) == 0 {
			delete(a.active, waterway)
			a.clearWaitLocked(waterway)
		} else {
			a.active[waterway] = kept
		}
		a.forgetCycleLocked(waterway, cycleID)
	}
	for waterway := range a.waiting {
		a.forgetCycleLocked(waterway, cycleID)
	}
	return released
}

// Current returns the active occupancies of a waterway.
func (a *Arbiter) Current(waterway WaterwayID) []Occupancy {
	a.mu.Lock()
	result := append([]Occupancy(nil), a.active[waterway]...)
	a.mu.Unlock()
	for index := range result {
		result[index] = result[index].clone()
	}
	return result
}

// Views reports every waterway with an occupant or a waitlist.
func (a *Arbiter) Views() []View {
	a.mu.Lock()
	defer a.mu.Unlock()
	seen := make(map[WaterwayID]struct{}, len(a.active)+len(a.waiting))
	for waterway := range a.active {
		seen[waterway] = struct{}{}
	}
	for waterway := range a.waiting {
		seen[waterway] = struct{}{}
	}
	now := a.now().UTC()
	result := make([]View, 0, len(seen))
	for waterway := range seen {
		view := View{WaterwayID: waterway, Occupants: []Occupancy{}, Waitlist: []Denial{}, EvaluatedAt: now}
		occupants := a.active[waterway]
		for index := range occupants {
			view.Occupants = append(view.Occupants, occupants[index].clone())
		}
		sort.Slice(view.Occupants, func(i, j int) bool {
			return view.Occupants[i].GrantedAt.Before(view.Occupants[j].GrantedAt)
		})
		view.Waitlist = append([]Denial(nil), a.waiting[waterway]...)
		result = append(result, view)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].WaterwayID < result[j].WaterwayID })
	return result
}

// Restore rebuilds an occupancy from the durable journal without re-evaluating.
func (a *Arbiter) Restore(occupancy Occupancy) {
	a.mu.Lock()
	a.upsertLocked(occupancy)
	a.mu.Unlock()
}

func (a *Arbiter) invalid(req Request, err error) *Denial {
	return &Denial{
		Code: "invalid_request", Reason: err.Error(), WaterwayID: req.WaterwayID,
		RequestedBy: req.ChamberID, Direction: req.Direction, ObservedAt: a.now().UTC(),
	}
}
