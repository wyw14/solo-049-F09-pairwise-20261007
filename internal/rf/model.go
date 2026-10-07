package rf

import "time"

type Polarization string

const (
	LeftCircular  Polarization = "LHCP"
	RightCircular Polarization = "RHCP"
	Linear        Polarization = "linear"
)

// State is the live link configuration of one RF chain. It only changes when a
// plan is tuned for execution, never when a plan is merely staged.
type State struct {
	ChainID          string       `json:"chain_id"`
	PassID           string       `json:"pass_id,omitempty"`
	CenterHz         int64        `json:"center_hz"`
	DopplerHz        int64        `json:"doppler_hz"`
	PlanRevision     int64        `json:"plan_revision,omitempty"`
	WindowStart      time.Time    `json:"window_start,omitempty"`
	WindowEnd        time.Time    `json:"window_end,omitempty"`
	TunedAt          time.Time    `json:"tuned_at,omitempty"`
	Polarization     Polarization `json:"polarization"`
	TransmitEnabled  bool         `json:"transmit_enabled"`
	InhibitOperation string       `json:"inhibit_operation,omitempty"`
	UpdatedAt        time.Time    `json:"updated_at"`
}

// Plan is a plan submission. The full Doppler step schedule is scoped to the
// pass time window and tagged with a revision so stale submissions can be told
// apart from amendments.
type Plan struct {
	PassID       string        `json:"pass_id"`
	ChainID      string        `json:"chain_id"`
	CenterHz     int64         `json:"center_hz"`
	DopplerHz    int64         `json:"doppler_hz"`
	Polarization Polarization  `json:"polarization"`
	Revision     int64         `json:"revision"`
	WindowStart  time.Time     `json:"window_start"`
	WindowEnd    time.Time     `json:"window_end"`
	Steps        []DopplerStep `json:"doppler_steps"`
}

// DopplerPlan is the persisted schedule staged for one chain. It does not touch
// the live State until the chain is tuned at execution time.
type DopplerPlan struct {
	ChainID      string        `json:"chain_id"`
	PassID       string        `json:"pass_id"`
	Revision     int64         `json:"revision"`
	CenterHz     int64         `json:"center_hz"`
	Polarization Polarization  `json:"polarization"`
	WindowStart  time.Time     `json:"window_start"`
	WindowEnd    time.Time     `json:"window_end"`
	Steps        []DopplerStep `json:"steps"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}
