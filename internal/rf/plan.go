package rf

import (
	"fmt"
	"sync"
	"time"

	"orbitlink/internal/audit"
	"orbitlink/internal/store"
)

// acquisitionMargin brackets the immediate plan synthesized by the legacy
// Apply entry point so a receiver attempt begun moments later still lies inside
// the tune window.
const acquisitionMargin = 30 * time.Second

type Controller struct {
	mu       sync.RWMutex
	path     string
	planPath string
	states   map[string]State
	plans    map[string]DopplerPlan
	audit    *audit.Trail
}

func NewController(paths store.Paths, trail *audit.Trail, chainIDs []string) (*Controller, error) {
	controller := &Controller{
		path:     paths.Snapshot("rf-chains"),
		planPath: paths.Snapshot("rf-plans"),
		states:   make(map[string]State),
		plans:    make(map[string]DopplerPlan),
		audit:    trail,
	}
	var states []State
	found, err := store.ReadJSON(controller.path, &states)
	if err != nil {
		return nil, err
	}
	if found {
		for _, state := range states {
			controller.states[state.ChainID] = state
		}
	}
	var plans []DopplerPlan
	found, err = store.ReadJSON(controller.planPath, &plans)
	if err != nil {
		return nil, err
	}
	if found {
		for _, plan := range plans {
			controller.plans[plan.ChainID] = plan
		}
	}
	for _, id := range chainIDs {
		if _, exists := controller.states[id]; !exists {
			controller.states[id] = State{ChainID: id, Polarization: RightCircular}
		}
	}
	if err := controller.flushLocked(); err != nil {
		return nil, err
	}
	return controller, nil
}

// Apply keeps the legacy "apply an immediate offset" entry point: it stages a
// two-step plan around the current instant and tunes the chain straight away.
func (c *Controller) Apply(plan Plan) (State, error) {
	if plan.PassID == "" || plan.ChainID == "" || plan.CenterHz <= 0 {
		return State{}, fmt.Errorf("pass, RF chain and center frequency are required")
	}
	now := time.Now().UTC()
	revision := plan.Revision
	if revision < 1 {
		revision = 1
	}
	plan.Revision = revision
	plan.WindowStart = now.Add(-acquisitionMargin)
	plan.WindowEnd = now.Add(acquisitionMargin)
	if len(plan.Steps) == 0 {
		plan.Steps = []DopplerStep{
			{At: plan.WindowStart, OffsetHz: plan.DopplerHz},
			{At: plan.WindowEnd, OffsetHz: plan.DopplerHz},
		}
	}
	if _, err := c.SavePlan(plan); err != nil {
		return State{}, err
	}
	return c.TuneAt(plan.ChainID, now)
}

// SavePlan stages the full windowed Doppler schedule for a chain. The schedule
// may be amended freely before the pass is executed, but a stale revision can
// never overwrite a newer plan, and nothing can overwrite the link once the
// chain has started executing or while it actively serves another pass.
func (c *Controller) SavePlan(plan Plan) (DopplerPlan, error) {
	if plan.PassID == "" || plan.ChainID == "" || plan.CenterHz <= 0 {
		return DopplerPlan{}, fmt.Errorf("pass, RF chain and center frequency are required")
	}
	if plan.Revision < 1 {
		return DopplerPlan{}, fmt.Errorf("plan revision must be at least one")
	}
	if err := ValidateSchedule(plan.CenterHz, plan.WindowStart, plan.WindowEnd, plan.Steps); err != nil {
		return DopplerPlan{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, found := c.states[plan.ChainID]
	if !found {
		return DopplerPlan{}, fmt.Errorf("RF chain %s not found", plan.ChainID)
	}
	now := time.Now().UTC()
	if err := guardCurrentLink(state, plan, now); err != nil {
		return DopplerPlan{}, err
	}
	existing, hasExisting := c.plans[plan.ChainID]
	if hasExisting && existing.PassID == plan.PassID {
		if plan.Revision < existing.Revision {
			return DopplerPlan{}, conflict("plan revision %d is older than staged revision %d for pass %s", plan.Revision, existing.Revision, plan.PassID)
		}
		if state.TransmitEnabled {
			return DopplerPlan{}, conflict("pass %s is already transmitting; Doppler plan can no longer be revised", plan.PassID)
		}
		if linkStarted(state, plan.PassID) && plan.Revision == existing.Revision {
			return DopplerPlan{}, conflict("execution of pass %s already started; a new revision is required to amend the Doppler plan", plan.PassID)
		}
	}
	steps := append([]DopplerStep(nil), plan.Steps...)
	staged := DopplerPlan{
		ChainID:      plan.ChainID,
		PassID:       plan.PassID,
		Revision:     plan.Revision,
		CenterHz:     plan.CenterHz,
		Polarization: plan.Polarization,
		WindowStart:  plan.WindowStart.UTC(),
		WindowEnd:    plan.WindowEnd.UTC(),
		Steps:        steps,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if hasExisting && existing.PassID == plan.PassID {
		staged.CreatedAt = existing.CreatedAt
	}
	c.plans[plan.ChainID] = staged
	if err := c.flushPlansLocked(); err != nil {
		delete(c.plans, plan.ChainID)
		if hasExisting {
			c.plans[plan.ChainID] = existing
		}
		return DopplerPlan{}, err
	}
	_, _ = c.audit.Record(audit.Event{Component: "rf", Action: "plan.staged", Subject: plan.ChainID, Fields: map[string]any{
		"pass_id": plan.PassID, "revision": plan.Revision, "center_hz": plan.CenterHz,
		"window_start": plan.WindowStart, "window_end": plan.WindowEnd, "steps": len(steps),
	}})
	return staged, nil
}

// TuneAt takes the Doppler offset for the given instant from the staged plan
// and commits it to the live link. It refuses to touch the link when the
// instant falls outside the plan window, the interpolated offset exceeds the
// capture range, the plan revision is stale, or another pass owns the link.
func (c *Controller) TuneAt(chainID string, at time.Time) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, found := c.states[chainID]
	if !found {
		return State{}, fmt.Errorf("RF chain %s not found", chainID)
	}
	plan, found := c.plans[chainID]
	if !found {
		return State{}, fmt.Errorf("RF chain %s has no staged Doppler plan", chainID)
	}
	at = at.UTC()
	if at.Before(plan.WindowStart) || at.After(plan.WindowEnd) {
		return State{}, conflict("tune time %s is outside pass %s plan window %s..%s",
			at.Format(time.RFC3339), plan.PassID, plan.WindowStart.Format(time.RFC3339), plan.WindowEnd.Format(time.RFC3339))
	}
	if err := guardCurrentLink(state, Plan{PassID: plan.PassID, ChainID: chainID, Revision: plan.Revision}, at); err != nil {
		return State{}, err
	}
	offset := InterpolateDoppler(plan.Steps, at)
	if err := ValidateOffset(offset); err != nil {
		return State{}, conflict("%s; current link left unchanged", err.Error())
	}
	state.PassID = plan.PassID
	state.CenterHz = plan.CenterHz
	state.DopplerHz = offset
	state.PlanRevision = plan.Revision
	state.WindowStart = plan.WindowStart
	state.WindowEnd = plan.WindowEnd
	state.TunedAt = at
	state.Polarization = plan.Polarization
	state.UpdatedAt = time.Now().UTC()
	c.states[chainID] = state
	if err := c.flushLocked(); err != nil {
		return State{}, err
	}
	_, _ = c.audit.Record(audit.Event{Component: "rf", Action: "doppler.tuned", Subject: chainID, Fields: map[string]any{
		"pass_id": plan.PassID, "revision": plan.Revision, "at": at, "doppler_hz": offset,
	}})
	return state, nil
}

// guardCurrentLink rejects submissions that would overwrite a live link owned
// by another pass, or roll an in-progress pass back to an older revision.
func guardCurrentLink(state State, plan Plan, now time.Time) error {
	if state.PassID == "" || state.PassID == plan.PassID {
		if state.PassID == plan.PassID && state.PlanRevision > 0 && plan.Revision < state.PlanRevision {
			return conflict("plan revision %d is older than active link revision %d", plan.Revision, state.PlanRevision)
		}
		return nil
	}
	if state.TransmitEnabled {
		return conflict("chain is transmitting for pass %s; current link cannot be overwritten", state.PassID)
	}
	if !state.WindowEnd.IsZero() && !now.After(state.WindowEnd) && !now.Before(state.WindowStart) {
		return conflict("chain is executing pass %s until %s; current link cannot be overwritten", state.PassID, state.WindowEnd.Format(time.RFC3339))
	}
	return nil
}

// linkStarted reports whether the chain has already tuned for the pass, which
// closes the pre-launch amendment window.
func linkStarted(state State, passID string) bool {
	return state.PassID == passID && !state.TunedAt.IsZero()
}

func (c *Controller) EnableTransmit(chainID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, found := c.states[chainID]
	if !found {
		return fmt.Errorf("RF chain %s not found", chainID)
	}
	state.TransmitEnabled = true
	state.InhibitOperation = ""
	state.UpdatedAt = time.Now().UTC()
	c.states[chainID] = state
	return c.flushLocked()
}

func (c *Controller) State(chainID string) (State, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	state, found := c.states[chainID]
	return state, found
}

func (c *Controller) StagedPlan(chainID string) (DopplerPlan, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	plan, found := c.plans[chainID]
	return plan, found
}

func (c *Controller) List() []State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	values := make([]State, 0, len(c.states))
	for _, state := range c.states {
		values = append(values, state)
	}
	return values
}

func (c *Controller) flushLocked() error {
	values := make([]State, 0, len(c.states))
	for _, state := range c.states {
		values = append(values, state)
	}
	return store.WriteJSON(c.path, values)
}

func (c *Controller) flushPlansLocked() error {
	values := make([]DopplerPlan, 0, len(c.plans))
	for _, plan := range c.plans {
		values = append(values, plan)
	}
	return store.WriteJSON(c.planPath, values)
}
