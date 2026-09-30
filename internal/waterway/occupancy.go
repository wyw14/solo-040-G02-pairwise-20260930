package waterway

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

// WaterwayID is the shared pilotage water segment arbitrated between chambers.
type WaterwayID string

// SharedReach is the default approach waterway used by every chamber.
const SharedReach WaterwayID = "shared-approach"

func (w WaterwayID) valid() bool { return strings.TrimSpace(string(w)) != "" }

// ReasonCode explains why a waterway request cannot be cleared.
type ReasonCode string

const (
	ReasonOpposedHeading ReasonCode = "opposed_heading" // occupant sails a conflicting direction
)

// Occupancy is one convoy's active hold on a shared water segment.
type Occupancy struct {
	WaterwayID WaterwayID      `json:"waterway_id"`
	ChamberID  string          `json:"chamber_id"`
	CycleID    uuid.UUID       `json:"cycle_id"`
	PlanID     uuid.UUID       `json:"plan_id"`
	Direction  model.Direction `json:"direction"`
	Convoy     string          `json:"convoy"`
	Vessels    []model.Vessel  `json:"vessels,omitempty"`
	GrantedAt  time.Time       `json:"granted_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

func (o Occupancy) key() convoyKey {
	return convoyKey{CycleID: o.CycleID, PlanID: o.PlanID}
}

func (o Occupancy) clone() Occupancy {
	o.Vessels = append([]model.Vessel(nil), o.Vessels...)
	return o
}

// convoyKey identifies the transit that owns an occupancy across re-requests.
type convoyKey struct {
	CycleID uuid.UUID
	PlanID  uuid.UUID
}

// Request is a directional application for a shared water segment.
type Request struct {
	WaterwayID WaterwayID
	ChamberID  string
	CycleID    uuid.UUID
	PlanID     uuid.UUID
	Direction  model.Direction
	Convoy     string
	Vessels    []model.Vessel
}

func (r Request) key() convoyKey {
	return convoyKey{CycleID: r.CycleID, PlanID: r.PlanID}
}

func (r Request) validate() error {
	if !r.WaterwayID.valid() {
		return errWaterway("waterway id is required")
	}
	if strings.TrimSpace(r.ChamberID) == "" {
		return errWaterway("chamber id is required")
	}
	if r.CycleID == uuid.Nil || r.PlanID == uuid.Nil {
		return errWaterway("cycle and plan are required")
	}
	if !r.Direction.Valid() {
		return errWaterway("direction must be upstream or downstream")
	}
	return nil
}

// Denial records who blocks a request and why, for the operations page.
type Denial struct {
	Code            ReasonCode      `json:"code"`
	Reason          string          `json:"reason"`
	WaterwayID      WaterwayID      `json:"waterway_id"`
	RequestedBy     string          `json:"requested_by"`
	RequestedCycle  uuid.UUID       `json:"requested_cycle"`
	RequestedPlan   uuid.UUID       `json:"requested_plan"`
	Direction       model.Direction `json:"direction"`
	BlockingChamber string          `json:"blocking_chamber,omitempty"`
	BlockingCycle   uuid.UUID       `json:"blocking_cycle,omitempty"`
	BlockingConvoy  string          `json:"blocking_convoy,omitempty"`
	BlockingHeading model.Direction `json:"blocking_heading,omitempty"`
	ObservedAt      time.Time       `json:"observed_at"`
}

// Decision is the result of a clearance application.
type Decision struct {
	Allowed   bool       `json:"allowed"`
	Waterway  WaterwayID `json:"waterway_id"`
	Occupancy *Occupancy `json:"occupancy,omitempty"`
	Denial    *Denial    `json:"denial,omitempty"`
}

// View is the operator-facing state of one shared water segment.
type View struct {
	WaterwayID  WaterwayID  `json:"waterway_id"`
	Occupants   []Occupancy `json:"occupants"`
	Waitlist    []Denial    `json:"waitlist"`
	EvaluatedAt time.Time   `json:"evaluated_at"`
}
