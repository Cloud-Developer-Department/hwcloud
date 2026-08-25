package rest

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// writeSSE serialises evt as JSON and writes it as an SSE data frame.
// Each call writes "data: <json>\n\n" and flushes the response writer.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, evt SSEEvent) error {
	b, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	if err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// setSSEHeaders writes the standard headers for an SSE stream.
func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}
