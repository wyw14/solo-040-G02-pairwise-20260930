package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/waterway"
)

type waterwayRequestInput struct {
	WaterwayID string          `json:"waterway_id,omitempty"`
	Direction  model.Direction `json:"direction"`
}

// requestWaterwayHandler applies for the shared pilotage water when a convoy
// enters or leaves the chamber. Conflicting headings are refused with the
// structured block reason; the refusal itself populates the waitlist.
func requestWaterwayHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input waterwayRequestInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		chamberID := chi.URLParam(r, "chamberID")
		if !input.Direction.Valid() {
			writeError(w, http.StatusBadRequest, errors.New("invalid waterway direction"))
			return
		}
		cycleValue, ok := runtime.Cycles.Current(chamberID)
		if !ok || cycleValue.Stage.Terminal() {
			writeError(w, http.StatusConflict, errors.New("active transit not found"))
			return
		}
		plan, found := runtime.Planner.Get(cycleValue.PlanID)
		if !found {
			writeError(w, http.StatusConflict, errors.New("convoy plan not found for transit"))
			return
		}
		waterwayID := waterway.WaterwayID(input.WaterwayID)
		if waterwayID == "" {
			waterwayID = waterway.SharedReach
		}
		req := waterway.Request{
			WaterwayID: waterwayID, ChamberID: chamberID,
			CycleID: cycleValue.ID, PlanID: cycleValue.PlanID,
			Direction: input.Direction, Convoy: convoyLabel(plan), Vessels: plan.Vessels,
		}
		decision, err := runtime.Waterway.RequestClearance(r.Context(), req)
		if err != nil {
			var denied *waterway.DeniedError
			if errors.As(err, &denied) {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error": denied.Error(), "decision": decision, "denial": denied.Denial,
				})
				return
			}
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, decision)
	}
}

// releaseWaterwayHandler lets an operator safely release a chamber's hold;
// cancellation, emergency and confirmed departure release automatically.
func releaseWaterwayHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chamberID := chi.URLParam(r, "chamberID")
		cycleValue, ok := runtime.Cycles.Current(chamberID)
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("active transit not found"))
			return
		}
		records, err := runtime.Waterway.ReleaseCycle(r.Context(), chamberID, cycleValue.ID, waterway.CauseManual)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"released": records})
	}
}

// waterwayHandler reports occupant convoys and the block reasons for waiters.
func waterwayHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		views := runtime.Waterway.Views()
		found := false
		for _, view := range views {
			if view.WaterwayID == waterway.SharedReach {
				found = true
				break
			}
		}
		if !found {
			views = append([]waterway.View{{
				WaterwayID: waterway.SharedReach, Occupants: []waterway.Occupancy{},
				Waitlist: []waterway.Denial{}, EvaluatedAt: runtime.Now().UTC(),
			}}, views...)
		}
		writeJSON(w, http.StatusOK, map[string]any{"waterways": views})
	}
}

func convoyLabel(plan model.ConvoyPlan) string {
	if len(plan.Vessels) == 0 {
		return "convoy-" + plan.ID.String()[:8]
	}
	if len(plan.Vessels) == 1 {
		return plan.Vessels[0].Name
	}
	return plan.Vessels[0].Name + " +" + strconv.Itoa(len(plan.Vessels)-1)
}
