package rf

import (
	"errors"
	"testing"
	"time"

	"orbitlink/internal/audit"
	"orbitlink/internal/store"
)

func newTestController(t *testing.T) *Controller {
	t.Helper()
	paths := store.NewPaths(t.TempDir())
	trail := audit.NewTrail(store.NewAppendLog(paths))
	controller, err := NewController(paths, trail, []string{"RF-01", "RF-02"})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func linearSteps(start time.Time, end time.Time, first, last int64) []DopplerStep {
	return []DopplerStep{
		{At: start, OffsetHz: first},
		{At: end, OffsetHz: last},
	}
}

func isConflict(t *testing.T, err error) {
	t.Helper()
	var conflict *PlanConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected plan conflict, got %v", err)
	}
}

func TestSavePlanStagesScheduleWithoutTouchingLink(t *testing.T) {
	controller := newTestController(t)
	start := time.Now().UTC().Add(time.Minute)
	end := start.Add(8 * time.Minute)
	plan, err := controller.SavePlan(Plan{
		PassID: "pass-1", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular,
		Revision: 1, WindowStart: start, WindowEnd: end,
		Steps: linearSteps(start, end, -10_000, 40_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("staged plan should keep full Doppler steps, got %d", len(plan.Steps))
	}
	state, _ := controller.State("RF-01")
	if state.PassID != "" || state.DopplerHz != 0 {
		t.Fatalf("staging a plan must not tune the link: %#v", state)
	}
	if _, found := controller.StagedPlan("RF-01"); !found {
		t.Fatal("staged plan not retrievable")
	}
}

func TestPlanAmendmentsBeforeTransmit(t *testing.T) {
	controller := newTestController(t)
	start := time.Now().UTC().Add(time.Minute)
	end := start.Add(8 * time.Minute)
	submit := func(revision int64, first, last int64) error {
		_, err := controller.SavePlan(Plan{
			PassID: "pass-1", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular,
			Revision: revision, WindowStart: start, WindowEnd: end,
			Steps: linearSteps(start, end, first, last),
		})
		return err
	}
	if err := submit(1, -10_000, 40_000); err != nil {
		t.Fatal(err)
	}
	// A newer revision amends the schedule.
	if err := submit(2, -12_000, 42_000); err != nil {
		t.Fatalf("pre-launch amendment should be allowed: %v", err)
	}
	// A stale revision must never overwrite the newer plan.
	isConflict(t, submit(1, 0, 0))
	staged, _ := controller.StagedPlan("RF-01")
	if staged.Revision != 2 || staged.Steps[0].OffsetHz != -12_000 {
		t.Fatalf("stale revision overwrote current plan: %#v", staged)
	}
	// Execution starts: the chain tunes from revision 2.
	mid := start.Add(4 * time.Minute)
	state, err := controller.TuneAt("RF-01", mid)
	if err != nil {
		t.Fatal(err)
	}
	expected := InterpolateDoppler(staged.Steps, mid)
	if state.DopplerHz != expected || state.PlanRevision != 2 {
		t.Fatalf("tune took %d Hz (rev %d), want %d Hz rev 2", state.DopplerHz, state.PlanRevision, expected)
	}
	// Same revision after execution started is rejected; a newer revision still amends.
	isConflict(t, submit(2, -12_000, 42_000))
	if err := submit(3, -14_000, 44_000); err != nil {
		t.Fatalf("newer revision after tune should still amend before transmit: %v", err)
	}
	// Once transmitting, the schedule is frozen.
	if err := controller.EnableTransmit("RF-01"); err != nil {
		t.Fatal(err)
	}
	isConflict(t, submit(4, 0, 0))
}

func TestTuneAtRejectsOutOfWindowWithoutChangingLink(t *testing.T) {
	controller := newTestController(t)
	start := time.Now().UTC().Add(time.Minute)
	end := start.Add(8 * time.Minute)
	if _, err := controller.SavePlan(Plan{
		PassID: "pass-1", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular,
		Revision: 1, WindowStart: start, WindowEnd: end,
		Steps: linearSteps(start, end, -10_000, 40_000),
	}); err != nil {
		t.Fatal(err)
	}
	var target *PlanConflictError
	if _, err := controller.TuneAt("RF-01", start.Add(-time.Second)); !errors.As(err, &target) {
		t.Fatalf("tune before window should conflict: %v", err)
	}
	if _, err := controller.TuneAt("RF-01", end.Add(time.Second)); !errors.As(err, &target) {
		t.Fatalf("tune after window should conflict: %v", err)
	}
	state, _ := controller.State("RF-01")
	if state.PassID != "" || state.DopplerHz != 0 {
		t.Fatalf("rejected tune must leave current link untouched: %#v", state)
	}
	// In-window tune works and picks the offset for the exact instant.
	state, err := controller.TuneAt("RF-01", start.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if state.DopplerHz != 2_500 {
		t.Fatalf("interpolated offset = %d, want 2500", state.DopplerHz)
	}
}

func TestSavePlanRejectsOutOfCaptureRangeAndCoverage(t *testing.T) {
	controller := newTestController(t)
	start := time.Now().UTC().Add(time.Minute)
	end := start.Add(8 * time.Minute)
	_, err := controller.SavePlan(Plan{
		PassID: "pass-1", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular,
		Revision: 1, WindowStart: start, WindowEnd: end,
		Steps: linearSteps(start, end, -600_000, 40_000),
	})
	if err == nil {
		t.Fatal("offset beyond capture range must be rejected")
	}
	_, err = controller.SavePlan(Plan{
		PassID: "pass-1", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular,
		Revision: 1, WindowStart: start, WindowEnd: end,
		Steps: linearSteps(start.Add(time.Minute), end, -10_000, 40_000),
	})
	if err == nil {
		t.Fatal("steps that do not bracket the window start must be rejected")
	}
}

func TestPlanCannotOverwriteActiveLinkOfAnotherPass(t *testing.T) {
	controller := newTestController(t)
	// Pass A's window brackets the current instant, so its link is active.
	aStart := time.Now().UTC().Add(-time.Minute)
	aEnd := time.Now().UTC().Add(5 * time.Minute)
	if _, err := controller.SavePlan(Plan{
		PassID: "pass-A", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular,
		Revision: 1, WindowStart: aStart, WindowEnd: aEnd,
		Steps: linearSteps(aStart, aEnd, 1_000, 2_000),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.TuneAt("RF-01", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	bStart := aEnd.Add(time.Hour)
	bEnd := bStart.Add(8 * time.Minute)
	submitB := func() error {
		_, err := controller.SavePlan(Plan{
			PassID: "pass-B", ChainID: "RF-01", CenterHz: 2_400_000_000, Polarization: LeftCircular,
			Revision: 1, WindowStart: bStart, WindowEnd: bEnd,
			Steps: linearSteps(bStart, bEnd, 0, 100),
		})
		return err
	}
	isConflict(t, submitB())
	state, _ := controller.State("RF-01")
	if state.PassID != "pass-A" || state.CenterHz != 2_245_000_000 {
		t.Fatalf("active link was overwritten: %#v", state)
	}

	// A finished pass (window in the past) can hand the chain over.
	finishedStart := time.Now().UTC().Add(-10 * time.Minute)
	finishedEnd := time.Now().UTC().Add(-5 * time.Minute)
	other := newTestController(t)
	if _, err := other.SavePlan(Plan{
		PassID: "pass-A", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular,
		Revision: 1, WindowStart: finishedStart, WindowEnd: finishedEnd,
		Steps: linearSteps(finishedStart, finishedEnd, 1_000, 2_000),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := other.TuneAt("RF-01", finishedStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	nextStart := time.Now().UTC().Add(time.Hour)
	nextEnd := nextStart.Add(8 * time.Minute)
	if _, err := other.SavePlan(Plan{
		PassID: "pass-B", ChainID: "RF-01", CenterHz: 2_400_000_000, Polarization: LeftCircular,
		Revision: 1, WindowStart: nextStart, WindowEnd: nextEnd,
		Steps: linearSteps(nextStart, nextEnd, 0, 100),
	}); err != nil {
		t.Fatalf("finished pass should release the chain: %v", err)
	}
}

func TestPlansPersistAcrossRestart(t *testing.T) {
	paths := store.NewPaths(t.TempDir())
	trail := audit.NewTrail(store.NewAppendLog(paths))
	controller, err := NewController(paths, trail, []string{"RF-01"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(time.Minute)
	end := start.Add(8 * time.Minute)
	if _, err := controller.SavePlan(Plan{
		PassID: "pass-1", ChainID: "RF-01", CenterHz: 2_245_000_000, Polarization: RightCircular,
		Revision: 3, WindowStart: start, WindowEnd: end,
		Steps: linearSteps(start, end, -10_000, 40_000),
	}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewController(paths, trail, []string{"RF-01"})
	if err != nil {
		t.Fatal(err)
	}
	staged, found := reloaded.StagedPlan("RF-01")
	if !found || staged.Revision != 3 || len(staged.Steps) != 2 {
		t.Fatalf("staged plan did not survive restart: %#v found=%v", staged, found)
	}
	state, err := reloaded.TuneAt("RF-01", start.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("tune after restart failed: %v", err)
	}
	if state.PlanRevision != 3 {
		t.Fatalf("tuned revision = %d, want 3", state.PlanRevision)
	}
}

func TestLegacyApplyTunesImmediately(t *testing.T) {
	controller := newTestController(t)
	state, err := controller.Apply(Plan{PassID: "pass-1", ChainID: "RF-02", CenterHz: 2_245_000_000, DopplerHz: 12_000, Polarization: RightCircular})
	if err != nil {
		t.Fatal(err)
	}
	if state.DopplerHz != 12_000 || state.PassID != "pass-1" {
		t.Fatalf("legacy apply should tune immediately: %#v", state)
	}
	staged, found := controller.StagedPlan("RF-02")
	if !found || len(staged.Steps) != 2 {
		t.Fatal("legacy apply should also stage a windowed plan")
	}
}
