package waterway

import (
	"context"

	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// foldKey identifies one convoy occupancy while folding the journal.
type foldKey struct {
	waterway WaterwayID
	cycleID  [16]byte
}

// Recovery folds the complete per-chamber journals and re-applies every
// waterway grant that has no matching release, so in-flight occupancy survives
// a restart regardless of the chamber snapshot cursor.
type Recovery struct {
	store   journal.Store
	service *Service
}

func NewRecovery(store journal.Store, service *Service) *Recovery {
	return &Recovery{store: store, service: service}
}

// RestoreAll replays every chamber partition and rebuilds active occupancies.
func (r *Recovery) RestoreAll(ctx context.Context) ([]Occupancy, error) {
	chambers, err := r.store.Chambers(ctx)
	if err != nil {
		return nil, err
	}
	active := make(map[foldKey]Occupancy)
	for _, chamberID := range chambers {
		events, err := r.store.Events(ctx, chamberID, 0)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			switch event.Type {
			case model.EventWaterwayGranted:
				var occupancy Occupancy
				if err := event.Decode(&occupancy); err != nil {
					return nil, err
				}
				if occupancy.ChamberID == "" {
					occupancy.ChamberID = chamberID
				}
				active[foldKey{waterway: occupancy.WaterwayID, cycleID: occupancy.CycleID}] = occupancy
			case model.EventWaterwayReleased:
				var record ReleaseRecord
				if err := event.Decode(&record); err != nil {
					return nil, err
				}
				delete(active, foldKey{waterway: record.WaterwayID, cycleID: record.CycleID})
			}
		}
	}
	restored := make([]Occupancy, 0, len(active))
	for _, occupancy := range active {
		r.service.Restore(occupancy)
		restored = append(restored, occupancy)
	}
	return restored, nil
}
