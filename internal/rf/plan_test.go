package rf

import (
	"strings"
	"testing"
	"time"

	"orbitlink/internal/audit"
	"orbitlink/internal/store"
)

func TestApplyStoresWindowedDopplerProfile(t *testing.T) {
	controller, dir := newTestController(t)
	aos := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	los := aos.Add(8 * time.Minute)
	mid := aos.Add(4 * time.Minute)
	steps := []DopplerStep{{At: aos, OffsetHz: 40_000}, {At: mid, OffsetHz: 0}, {At: los, OffsetHz: -40_000}}
	state, err := controller.Apply(Plan{PassID: "PASS-1", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular, Window: Window{AOS: aos, LOS: los, Revision: 1}, Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.DopplerSteps) != 3 || state.Window.Revision != 1 {
		t.Fatalf("plan did not store the windowed profile: %#v", state)
	}
	for instant, want := range map[time.Time]int64{aos: 40_000, aos.Add(2 * time.Minute): 20_000, mid: 0, los: -40_000} {
		offset, found := controller.OffsetAt("RF-01", instant)
		if !found || offset != want {
			t.Fatalf("OffsetAt(%s) = %d, %v; want %d", instant, offset, found, want)
		}
	}
	reloaded, err := NewController(store.NewPaths(dir), controller.audit, []string{"RF-01"})
	if err != nil {
		t.Fatal(err)
	}
	persisted, found := reloaded.State("RF-01")
	if !found || len(persisted.DopplerSteps) != 3 || persisted.Window.Revision != 1 {
		t.Fatalf("windowed profile did not survive reload: %#v", persisted)
	}
	if offset, _ := reloaded.OffsetAt("RF-01", aos.Add(2*time.Minute)); offset != 20_000 {
		t.Fatalf("reloaded OffsetAt = %d, want 20000", offset)
	}
}

func TestApplySynthesizesFlatProfileAcrossWindow(t *testing.T) {
	controller, _ := newTestController(t)
	aos := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	los := aos.Add(8 * time.Minute)
	state, err := controller.Apply(Plan{PassID: "PASS-1", ChainID: "RF-01", CenterHz: 2_245_000_000, DopplerHz: 12_000, Polarization: RightCircular, Window: Window{AOS: aos, LOS: los, Revision: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.DopplerSteps) != 2 || !state.DopplerSteps[0].At.Equal(aos) || !state.DopplerSteps[1].At.Equal(los) {
		t.Fatalf("flat profile does not span the window: %#v", state.DopplerSteps)
	}
	if offset, _ := controller.OffsetAt("RF-01", aos.Add(4*time.Minute)); offset != 12_000 {
		t.Fatalf("flat profile offset = %d, want 12000", offset)
	}
}

func TestApplyAllowsRevisionBeforeTransmit(t *testing.T) {
	controller, _ := newTestController(t)
	aos := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	los := aos.Add(8 * time.Minute)
	apply := func(passID string, revision int64, offset int64) State {
		state, err := controller.Apply(Plan{PassID: passID, ChainID: "RF-01", CenterHz: 2_245_000_000, DopplerHz: offset, Polarization: RightCircular, Window: Window{AOS: aos, LOS: los, Revision: revision}})
		if err != nil {
			t.Fatalf("revision %d for %s rejected: %v", revision, passID, err)
		}
		return state
	}
	apply("PASS-1", 1, 10_000)
	state := apply("PASS-1", 2, 20_000)
	if state.Window.Revision != 2 {
		t.Fatalf("newer revision not stored: %#v", state.Window)
	}
	if offset, _ := controller.OffsetAt("RF-01", aos); offset != 20_000 {
		t.Fatalf("revised profile offset = %d, want 20000", offset)
	}
	state = apply("PASS-2", 1, 30_000)
	if state.PassID != "PASS-2" {
		t.Fatalf("plan for a new pass did not supersede: %#v", state)
	}
}

func TestApplyDoesNotOverwriteCurrentLink(t *testing.T) {
	aos := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	los := aos.Add(8 * time.Minute)
	window := Window{AOS: aos, LOS: los, Revision: 2}
	seedSteps := []DopplerStep{{At: aos, OffsetHz: 40_000}, {At: los, OffsetHz: -40_000}}
	seed := Plan{PassID: "PASS-1", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular, Window: window, Steps: seedSteps}
	cases := []struct {
		name   string
		plan   Plan
		want   string
		enable bool
	}{
		{name: "offset beyond capture range", plan: Plan{PassID: "PASS-1", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular, Window: window, Steps: []DopplerStep{{At: aos, OffsetHz: 40_000}, {At: los, OffsetHz: 600_000}}}, want: "capture range"},
		{name: "steps outside pass window", plan: Plan{PassID: "PASS-1", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular, Window: window, Steps: []DopplerStep{{At: aos, OffsetHz: 40_000}, {At: los.Add(-time.Minute), OffsetHz: -40_000}}}, want: "cover the pass window"},
		{name: "stale window revision", plan: Plan{PassID: "PASS-1", ChainID: "RF-01", CenterHz: 2_245_000_000, DopplerHz: 99_000, Polarization: RightCircular, Window: Window{AOS: aos, LOS: los, Revision: 1}}, want: "older than current revision"},
		{name: "revision after transmit enabled", enable: true, plan: Plan{PassID: "PASS-1", ChainID: "RF-01", CenterHz: 2_245_000_000, DopplerHz: 99_000, Polarization: RightCircular, Window: Window{AOS: aos, LOS: los, Revision: 3}}, want: "before transmit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controller, _ := newTestController(t)
			if _, err := controller.Apply(seed); err != nil {
				t.Fatal(err)
			}
			if tc.enable {
				if err := controller.EnableTransmit("RF-01"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := controller.Apply(tc.plan); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Apply error = %v, want substring %q", err, tc.want)
			}
			state, found := controller.State("RF-01")
			if !found {
				t.Fatal("chain state missing")
			}
			if state.Window.Revision != 2 || len(state.DopplerSteps) != 2 || state.DopplerSteps[0].OffsetHz != 40_000 {
				t.Fatalf("current link was overwritten: %#v", state)
			}
			if offset, _ := controller.OffsetAt("RF-01", aos); offset != 40_000 {
				t.Fatalf("current link offset changed to %d", offset)
			}
		})
	}
}

func newTestController(t *testing.T) (*Controller, string) {
	t.Helper()
	dir := t.TempDir()
	paths := store.NewPaths(dir)
	trail := audit.NewTrail(store.NewAppendLog(paths))
	controller, err := NewController(paths, trail, []string{"RF-01"})
	if err != nil {
		t.Fatal(err)
	}
	return controller, dir
}
