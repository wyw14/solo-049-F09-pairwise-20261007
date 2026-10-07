package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stationclock "orbitlink/internal/clock"
	passsvc "orbitlink/internal/pass"
	"orbitlink/internal/receiver"
	"orbitlink/internal/rf"
	"orbitlink/internal/satellite"
	"orbitlink/internal/weather"
)

func TestOperationsPagesAndAPIsUseRuntimeState(t *testing.T) {
	runtime := newTestRuntime(t)
	router, err := NewRouter(runtime)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	for _, target := range []string{"/passes", "/antennas", "/rf-chains", "/recordings", "/healthz", "/api/passes", "/api/antennas", "/api/rf-chains", "/api/recordings"} {
		response, err := server.Client().Get(server.URL + target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned %d", target, response.StatusCode)
		}
		_ = response.Body.Close()
	}
}

func TestPassRevisionAcquisitionAndRecordingFlow(t *testing.T) {
	runtime := newTestRuntime(t)
	value := runtime.Passes.List()[0]
	if _, err := runtime.RF.Apply(rf.Plan{PassID: value.ID, ChainID: "RF-01", CenterHz: 2_245_000_000, DopplerHz: 12_000, Polarization: rf.RightCircular}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.RF.Apply(rf.Plan{PassID: value.ID, ChainID: "RF-02", CenterHz: 2_245_000_000, DopplerHz: 12_050, Polarization: rf.RightCircular}); err != nil {
		t.Fatal(err)
	}
	attempt, err := runtime.Attempts.Begin(value.ID, "RF-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Passes.BeginAcquisition(value.ID, attempt.ID); err != nil {
		t.Fatal(err)
	}
	results := []receiver.AcquisitionResult{
		{PassID: value.ID, AttemptID: attempt.ID, ChainID: "RF-01", Locked: true, Synced: true, Epoch: 7},
		{PassID: value.ID, AttemptID: attempt.ID, ChainID: "RF-02", Locked: true, Synced: true, Epoch: 7},
	}
	decision, err := runtime.Passes.AcceptDiversity(value.ID, results)
	if err != nil || !decision.Ready {
		t.Fatalf("diversity did not converge: %#v %v", decision, err)
	}
	active, found := runtime.Recorder.ActiveForPass(value.ID)
	if !found {
		t.Fatal("tracking did not start a recorder")
	}
	session, err := runtime.NewDecoderSession(value.ID, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.OnLock(attempt.ID); err != nil {
		t.Fatal(err)
	}
	if written, err := session.Feed(attempt.ID, make([]byte, 128)); err != nil || written != 2 {
		t.Fatalf("decoder feed wrote %d frames: %v", written, err)
	}
	if _, err := runtime.Recorder.Drain(context.Background(), active.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Recorder.Seal(active.ID, "integration complete"); err != nil {
		t.Fatal(err)
	}
}

func TestEphemerisClockAndWindSafetyFlow(t *testing.T) {
	runtime := newTestRuntime(t)
	value := runtime.Passes.List()[0]
	previous, _ := runtime.Ephemerides.Current(value.SatelliteID)
	if _, err := runtime.Ephemerides.Import(satellite.Ephemeris{SatelliteID: value.SatelliteID, Revision: previous.Revision + 1, Epoch: time.Now().UTC(), Line1: "revised orbital element one", Line2: "revised orbital element two"}); err != nil {
		t.Fatal(err)
	}
	commands := runtime.Commands.List(value.ID)
	if len(commands) == 0 || commands[len(commands)-1].Revision != previous.Revision+1 {
		t.Fatal("revised pointing commands were not queued")
	}
	if _, err := runtime.Clock.Apply(stationclock.Sample{ObservedAt: time.Now().UTC(), Offset: 2 * time.Second, Source: "gps", Quality: "locked"}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.Clock.Corrections()) != 1 {
		t.Fatal("clock correction was not persisted")
	}
	if err := runtime.RF.EnableTransmit("RF-01"); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Weather.Handle(context.Background(), weather.WindSample{StationID: "station", GustMetersPerSecond: 25, ObservedAt: time.Now().UTC()}, value.ID, value.AntennaID, "RF-01")
	if err != nil || !result.InhibitConfirmed || !result.Stowed {
		t.Fatalf("wind safety did not converge: %#v %v", result, err)
	}
}

func TestRouterRejectsOverlappingReservation(t *testing.T) {
	runtime := newTestRuntime(t)
	first := runtime.Passes.List()[0]
	second, err := runtime.Passes.Create(passsvc.CreateRequest{SatelliteID: "SAT-C07", AntennaID: first.AntennaID, RFChains: []string{"RF-03"}, RequiredReceiverChains: []string{"RF-03"}, AOS: first.Window.AOS.Add(time.Minute), LOS: first.Window.LOS.Add(time.Minute), Revision: 1, OwnerStation: "north"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.WindowRecompute.Apply(second.ID, second.Window); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Reservations.ReservePass(second.ID, second.AntennaID); err == nil {
		t.Fatal("overlapping reservation unexpectedly succeeded")
	}
}

func TestRFPlanStagesRevisesAndTunesAtInstant(t *testing.T) {
	runtime := newTestRuntime(t)
	pass := runtime.Passes.List()[0]
	start, end := pass.Window.AOS, pass.Window.LOS
	payload := func(revision int64) string {
		return fmt.Sprintf(`{"pass_id":%q,"center_hz":2245000000,"polarization":"RHCP","revision":%d,`+
			`"doppler_steps":[{"at":%q,"offset_hz":-10000},{"at":%q,"offset_hz":40000}]}`,
			pass.ID, revision, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
	}
	router, err := NewRouter(runtime)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()

	if response := requestJSON(t, server.Client(), "POST", server.URL+"/api/rf-chains/RF-01/plan", payload(1)); response.StatusCode != http.StatusOK {
		t.Fatalf("stage revision 1: status %d", response.StatusCode)
		response.Body.Close()
	}
	// Staging must not tune the live link.
	if state, found := runtime.RF.State("RF-01"); !found || state.DopplerHz != 0 || state.PassID != "" {
		t.Fatalf("staging a plan changed the live link: %#v", state)
	}
	// A newer revision amends the schedule.
	if response := requestJSON(t, server.Client(), "POST", server.URL+"/api/rf-chains/RF-01/plan", payload(2)); response.StatusCode != http.StatusOK {
		t.Fatalf("amend revision 2: status %d", response.StatusCode)
	} else {
		response.Body.Close()
	}
	// An older revision must not overwrite the current plan: 409, plan untouched.
	if response := requestJSON(t, server.Client(), "POST", server.URL+"/api/rf-chains/RF-01/plan", payload(1)); response.StatusCode != http.StatusConflict {
		t.Fatalf("stale revision: status %d, want 409", response.StatusCode)
	} else {
		response.Body.Close()
	}
	if plan, _ := runtime.RF.StagedPlan("RF-01"); plan.Revision != 2 {
		t.Fatalf("stale revision overwrote current plan: rev %d", plan.Revision)
	}
	// A tune before the pass window must be rejected with 409 and not tune.
	early := fmt.Sprintf(`{"at":%q}`, start.Add(-time.Minute).Format(time.RFC3339Nano))
	if response := requestJSON(t, server.Client(), "POST", server.URL+"/api/rf-chains/RF-01/tune", early); response.StatusCode != http.StatusConflict {
		t.Fatalf("out-of-window tune: status %d, want 409", response.StatusCode)
	} else {
		response.Body.Close()
	}
	// In-window tune takes the offset for that exact instant.
	instant := start.Add(4 * time.Minute)
	at := fmt.Sprintf(`{"at":%q}`, instant.Format(time.RFC3339Nano))
	response := requestJSON(t, server.Client(), "POST", server.URL+"/api/rf-chains/RF-01/tune", at)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("in-window tune: status %d", response.StatusCode)
	}
	var state rf.State
	decodeBody(t, response, &state)
	plan, found := runtime.RF.StagedPlan("RF-01")
	if !found {
		t.Fatal("staged plan missing")
	}
	if want := rf.InterpolateDoppler(plan.Steps, instant); state.DopplerHz != want || state.PlanRevision != 2 {
		t.Fatalf("tuned %d Hz (rev %d), want %d Hz rev 2", state.DopplerHz, state.PlanRevision, want)
	}
}

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	runtime, err := NewRuntime(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func decodeBody(t *testing.T, response *http.Response, value any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(value); err != nil {
		t.Fatal(err)
	}
}

func requestJSON(t *testing.T, client *http.Client, method, target, payload string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, target, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
