package rf

import (
	"fmt"
	"time"
)

type Polarization string

const (
	LeftCircular  Polarization = "LHCP"
	RightCircular Polarization = "RHCP"
	Linear        Polarization = "linear"
)

// Window binds a frequency plan to one revision of the pass visibility window.
type Window struct {
	AOS      time.Time `json:"aos"`
	LOS      time.Time `json:"los"`
	Revision int64     `json:"revision"`
}

func (w Window) Validate() error {
	if w.Revision < 1 || w.AOS.IsZero() || !w.LOS.After(w.AOS) {
		return fmt.Errorf("plan window needs one revision and increasing boundaries")
	}
	return nil
}

type State struct {
	ChainID          string        `json:"chain_id"`
	PassID           string        `json:"pass_id,omitempty"`
	CenterHz         int64         `json:"center_hz"`
	DopplerHz        int64         `json:"doppler_hz"`
	DopplerSteps     []DopplerStep `json:"doppler_steps,omitempty"`
	Window           Window        `json:"window,omitempty"`
	Polarization     Polarization  `json:"polarization"`
	TransmitEnabled  bool          `json:"transmit_enabled"`
	InhibitOperation string        `json:"inhibit_operation,omitempty"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

// OffsetAt samples the stored Doppler profile at one instant, falling back to
// the last applied offset when the chain holds no stepped profile.
func (s State) OffsetAt(at time.Time) int64 {
	if len(s.DopplerSteps) == 0 {
		return s.DopplerHz
	}
	return InterpolateDoppler(s.DopplerSteps, at)
}

type Plan struct {
	PassID       string        `json:"pass_id"`
	ChainID      string        `json:"chain_id"`
	CenterHz     int64         `json:"center_hz"`
	DopplerHz    int64         `json:"doppler_hz"`
	Steps        []DopplerStep `json:"doppler_steps,omitempty"`
	Window       Window        `json:"window"`
	Polarization Polarization  `json:"polarization"`
}
