package api

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"orbitlink/internal/rf"
	"orbitlink/internal/satellite"
	"orbitlink/internal/weather"
)

func (r *Runtime) listAntennas(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"items": r.Antennas.List()})
}
func (r *Runtime) listRFChains(writer http.ResponseWriter, _ *http.Request) {
	chains := r.RF.List()
	plans := make(map[string]rf.DopplerPlan, len(chains))
	for _, chain := range chains {
		if plan, found := r.RF.StagedPlan(chain.ChainID); found {
			plans[chain.ChainID] = plan
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": chains, "plans": plans})
}

func (r *Runtime) applyRFPlan(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		PassID       string           `json:"pass_id"`
		CenterHz     int64            `json:"center_hz"`
		DopplerHz    int64            `json:"doppler_hz"`
		Polarization rf.Polarization  `json:"polarization"`
		Revision     int64            `json:"revision"`
		WindowStart  time.Time        `json:"window_start"`
		WindowEnd    time.Time        `json:"window_end"`
		DopplerSteps []rf.DopplerStep `json:"doppler_steps"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, err)
		return
	}
	revision, windowStart, windowEnd := resolvePlanWindow(r, input.PassID, input.Revision, input.WindowStart, input.WindowEnd)
	steps := input.DopplerSteps
	if len(steps) == 0 {
		steps = []rf.DopplerStep{{At: windowStart, OffsetHz: input.DopplerHz}, {At: windowEnd, OffsetHz: input.DopplerHz}}
	}
	plan, err := r.RF.SavePlan(rf.Plan{
		PassID: input.PassID, ChainID: chi.URLParam(request, "chainID"), CenterHz: input.CenterHz,
		DopplerHz: input.DopplerHz, Polarization: input.Polarization, Revision: revision,
		WindowStart: windowStart, WindowEnd: windowEnd, Steps: steps,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	state, _ := r.RF.State(chi.URLParam(request, "chainID"))
	writeJSON(writer, http.StatusOK, map[string]any{"plan": plan, "state": state})
}

// resolvePlanWindow fills the plan revision and time window from the scheduled
// pass when the caller does not send them explicitly.
func resolvePlanWindow(r *Runtime, passID string, revision int64, start, end time.Time) (int64, time.Time, time.Time) {
	if passID != "" {
		if value, found := r.Passes.Get(passID); found {
			if revision < 1 {
				revision = value.Window.Revision
			}
			if start.IsZero() {
				start = value.Window.AOS
			}
			if end.IsZero() {
				end = value.Window.LOS
			}
		}
	}
	return revision, start, end
}

// tuneRFChain commits the Doppler offset for the requested instant (the station
// clock's now by default) from the staged plan to the live link.
func (r *Runtime) tuneRFChain(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		At time.Time `json:"at"`
	}
	if err := decodeJSON(request, &input); err != nil && !errors.Is(err, io.EOF) {
		writeError(writer, err)
		return
	}
	at := input.At
	if at.IsZero() {
		at = r.Clock.Now()
	}
	state, err := r.RF.TuneAt(chi.URLParam(request, "chainID"), at)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, state)
}

func (r *Runtime) enableTransmit(writer http.ResponseWriter, request *http.Request) {
	if err := r.RF.EnableTransmit(chi.URLParam(request, "chainID")); err != nil {
		writeError(writer, err)
		return
	}
	state, _ := r.RF.State(chi.URLParam(request, "chainID"))
	writeJSON(writer, http.StatusOK, state)
}

func (r *Runtime) executeDuePointing(writer http.ResponseWriter, _ *http.Request) {
	commands, err := r.Executor.RunDue(time.Now().UTC())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"executed": commands})
}

func (r *Runtime) importEphemeris(writer http.ResponseWriter, request *http.Request) {
	var input satellite.Ephemeris
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, err)
		return
	}
	value, err := r.Ephemerides.Import(input)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, value)
}

func (r *Runtime) stowAntenna(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		PassID              string  `json:"pass_id"`
		ChainID             string  `json:"chain_id"`
		GustMetersPerSecond float64 `json:"gust_meters_per_second"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, err)
		return
	}
	result, err := r.Weather.Handle(request.Context(), weather.WindSample{StationID: "orbitlink", GustMetersPerSecond: input.GustMetersPerSecond, ObservedAt: time.Now().UTC()}, input.PassID, chi.URLParam(request, "antennaID"), input.ChainID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}
