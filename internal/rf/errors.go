package rf

import "fmt"

// PlanConflictError marks a submission that must not change the current link:
// stale revision, amendment after execution started, or an active link owned
// by another pass. The API layer maps it to HTTP 409.
type PlanConflictError struct {
	Reason string
}

func (e *PlanConflictError) Error() string {
	return e.Reason
}

func conflict(format string, args ...any) error {
	return &PlanConflictError{Reason: fmt.Sprintf(format, args...)}
}

// Conflict builds a PlanConflictError from callers outside the package.
func Conflict(format string, args ...any) *PlanConflictError {
	return &PlanConflictError{Reason: fmt.Sprintf(format, args...)}
}
