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

// MaxDopplerOffsetHz bounds how far the receiver can be pulled off the center
// frequency. Anything outside the capture range can never be tuned.
const MaxDopplerOffsetHz int64 = 500_000

// ValidateDopplerPlan validates a bare center/step pair for submissions that do
// not carry a pass window yet (the steps are synthesized around "now").
func ValidateDopplerPlan(centerHz int64, steps []DopplerStep) error {
	if centerHz <= 0 || len(steps) < 2 {
		return fmt.Errorf("frequency plan requires a center and at least two steps")
	}
	if !sort.SliceIsSorted(steps, func(i, j int) bool { return steps[i].At.Before(steps[j].At) }) {
		return fmt.Errorf("Doppler steps must be ordered")
	}
	for _, step := range steps {
		if err := ValidateOffset(step.OffsetHz); err != nil {
			return err
		}
	}
	return nil
}

// ValidateSchedule validates a windowed Doppler schedule: every offset must lie
// inside the receiver capture range and the steps must bracket the whole pass
// window so execution never has to extrapolate.
func ValidateSchedule(centerHz int64, windowStart, windowEnd time.Time, steps []DopplerStep) error {
	if err := ValidateDopplerPlan(centerHz, steps); err != nil {
		return err
	}
	if windowStart.IsZero() || !windowEnd.After(windowStart) {
		return fmt.Errorf("Doppler plan needs a pass time window with increasing boundaries")
	}
	if steps[0].At.After(windowStart) {
		return fmt.Errorf("Doppler steps must cover the start of the pass window at %s", windowStart.Format(time.RFC3339))
	}
	if steps[len(steps)-1].At.Before(windowEnd) {
		return fmt.Errorf("Doppler steps must cover the end of the pass window at %s", windowEnd.Format(time.RFC3339))
	}
	return nil
}

func ValidateOffset(offsetHz int64) error {
	if offsetHz > MaxDopplerOffsetHz || offsetHz < -MaxDopplerOffsetHz {
		return fmt.Errorf("Doppler offset %d Hz exceeds receiver capture range of ±%d Hz", offsetHz, MaxDopplerOffsetHz)
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
