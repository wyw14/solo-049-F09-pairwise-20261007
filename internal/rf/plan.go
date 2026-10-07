package rf

import (
	"fmt"
	"sync"
	"time"

	"orbitlink/internal/audit"
	"orbitlink/internal/store"
)

type Controller struct {
	mu     sync.RWMutex
	path   string
	states map[string]State
	audit  *audit.Trail
}

func NewController(paths store.Paths, trail *audit.Trail, chainIDs []string) (*Controller, error) {
	controller := &Controller{path: paths.Snapshot("rf-chains"), states: make(map[string]State), audit: trail}
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

// Apply stores the complete Doppler profile for one revision of the pass
// window. Plans may be revised until transmit is enabled; out-of-range or
// stale-revision plans are rejected without touching the current link.
func (c *Controller) Apply(plan Plan) (State, error) {
	if plan.PassID == "" || plan.ChainID == "" || plan.CenterHz <= 0 {
		return State{}, fmt.Errorf("pass, RF chain and center frequency are required")
	}
	if err := plan.Window.Validate(); err != nil {
		return State{}, err
	}
	steps := plan.Steps
	if len(steps) == 0 {
		steps = []DopplerStep{{At: plan.Window.AOS, OffsetHz: plan.DopplerHz}, {At: plan.Window.LOS, OffsetHz: plan.DopplerHz}}
	}
	if err := ValidateDopplerPlan(plan.CenterHz, steps); err != nil {
		return c.rejectPlan(plan, err.Error())
	}
	if err := ValidateWindowCoverage(plan.Window, steps); err != nil {
		return c.rejectPlan(plan, err.Error())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, found := c.states[plan.ChainID]
	if !found {
		return State{}, fmt.Errorf("RF chain %s not found", plan.ChainID)
	}
	if state.TransmitEnabled {
		return c.rejectPlan(plan, "transmit already enabled; plan revisions must land before transmit")
	}
	if plan.PassID == state.PassID && plan.Window.Revision < state.Window.Revision {
		return c.rejectPlan(plan, fmt.Sprintf("plan revision %d is older than current revision %d; refusing to overwrite the current link", plan.Window.Revision, state.Window.Revision))
	}
	state.PassID = plan.PassID
	state.CenterHz = plan.CenterHz
	state.Window = plan.Window
	state.DopplerSteps = append([]DopplerStep(nil), steps...)
	state.DopplerHz = InterpolateDoppler(steps, time.Now().UTC())
	state.Polarization = plan.Polarization
	state.UpdatedAt = time.Now().UTC()
	c.states[plan.ChainID] = state
	if err := c.flushLocked(); err != nil {
		return State{}, err
	}
	_, _ = c.audit.Record(audit.Event{Component: "rf", Action: "plan.applied", Subject: plan.ChainID, Fields: map[string]any{"pass_id": plan.PassID, "center_hz": plan.CenterHz, "doppler_hz": state.DopplerHz, "window_revision": plan.Window.Revision, "doppler_steps": len(steps)}})
	return state, nil
}

func (c *Controller) rejectPlan(plan Plan, reason string) (State, error) {
	_, _ = c.audit.Record(audit.Event{Component: "rf", Action: "plan.rejected", Subject: plan.ChainID, Severity: audit.Warning, Fields: map[string]any{"pass_id": plan.PassID, "window_revision": plan.Window.Revision, "reason": reason}})
	return State{}, fmt.Errorf("%s", reason)
}

// OffsetAt samples the chain's stored Doppler profile at one instant. It is
// the execution-time view of the plan: every consumer reads the offset for
// its own timestamp instead of a value frozen when the plan was applied.
func (c *Controller) OffsetAt(chainID string, at time.Time) (int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	state, found := c.states[chainID]
	if !found {
		return 0, false
	}
	return state.OffsetAt(at), true
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
