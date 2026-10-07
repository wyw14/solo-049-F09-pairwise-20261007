package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"orbitlink/internal/rf"
	"orbitlink/internal/scheduler"
)

func decodeJSON(request *http.Request, value any) error {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	var conflict *scheduler.ConflictError
	if errors.As(err, &conflict) {
		status = http.StatusConflict
	}
	var planConflict *rf.PlanConflictError
	if errors.As(err, &planConflict) {
		status = http.StatusConflict
	}
	writeJSON(writer, status, map[string]any{"error": err.Error(), "status": status})
}
