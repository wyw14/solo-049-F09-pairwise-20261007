package rf

import (
	"fmt"
	"sort"
	"time"
)

type DopplerStep struct {
	At       time.Time `json:"at"`
	OffsetHz int64     `json:"offset_hz"`
}

func ValidateDopplerPlan(centerHz int64, steps []DopplerStep) error {
	if centerHz <= 0 || len(steps) < 2 {
		return fmt.Errorf("frequency plan requires a center and at least two steps")
	}
	if !sort.SliceIsSorted(steps, func(i, j int) bool { return steps[i].At.Before(steps[j].At) }) {
		return fmt.Errorf("Doppler steps must be ordered")
	}
	for _, step := range steps {
		if step.OffsetHz > 500_000 || step.OffsetHz < -500_000 {
			return fmt.Errorf("Doppler offset exceeds receiver capture range")
		}
	}
	return nil
}

// ValidateWindowCoverage requires the stepped profile to span the whole pass
// window so execution-time sampling never has to extrapolate.
func ValidateWindowCoverage(window Window, steps []DopplerStep) error {
	if len(steps) == 0 {
		return fmt.Errorf("Doppler steps are required")
	}
	if steps[0].At.After(window.AOS) || steps[len(steps)-1].At.Before(window.LOS) {
		return fmt.Errorf("Doppler steps must cover the pass window")
	}
	return nil
}

func InterpolateDoppler(steps []DopplerStep, at time.Time) int64 {
	if len(steps) == 0 {
		return 0
	}
	for index := 1; index < len(steps); index++ {
		if at.Before(steps[index].At) {
			left, right := steps[index-1], steps[index]
			span := right.At.Sub(left.At)
			if span <= 0 {
				return left.OffsetHz
			}
			ratio := float64(at.Sub(left.At)) / float64(span)
			return left.OffsetHz + int64(float64(right.OffsetHz-left.OffsetHz)*ratio)
		}
	}
	return steps[len(steps)-1].OffsetHz
}
