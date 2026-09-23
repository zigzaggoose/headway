package api

import (
	"encoding/json"
	"net/http"
)

type errorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
	Reasons []string `json:"reasons,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // status already sent: a failure here is a client that left
}

// writeError renders the single error envelope of §7.2.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, reasons ...string) {
	var b errorBody
	b.Error.Code = code
	b.Error.Message = message
	b.Error.RequestID = requestID(r.Context())
	b.Reasons = reasons
	writeJSON(w, status, b)
}
