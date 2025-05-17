package nats2sse

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

// SSEEvent represents a Server-Sent Event.
type SSEEvent struct {
	ID    string // Optional: event ID
	Event string // Optional: event type
	Data  string // Data payload (should be UTF-8 or base64 encoded if binary)
	Retry uint   // Optional: retry timeout in milliseconds
}

// SSEWriter is a helper to write SSE formatted messages to an http.ResponseWriter.
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewSSEWriter creates a new SSEWriter.
// It sets the required SSE headers on the http.ResponseWriter.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming unsupported: ResponseWriter is not a Flusher")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// w.Header().Set("Access-Control-Allow-Origin", "*") // Optional: CORS

	return &SSEWriter{w: w, flusher: flusher}, nil
}

// writeField writes a single field of an SSE message.
func (sw *SSEWriter) writeField(name, value string) error {
	if value == "" { // Don't write empty fields unless it's data (which is handled specially)
		return nil
	}
	_, err := fmt.Fprintf(sw.w, "%s: %s\n", name, value)
	return err
}

// SendEvent sends a structured SSEEvent.
// Data is sent as is; if it contains newlines, it will be split into multiple 'data:' lines.
func (sw *SSEWriter) SendEvent(event SSEEvent) error {
	if err := sw.writeField("id", event.ID); err != nil {
		return err
	}
	if err := sw.writeField("event", event.Event); err != nil {
		return err
	}
	if event.Retry > 0 {
		if err := sw.writeField("retry", fmt.Sprintf("%d", event.Retry)); err != nil {
			return err
		}
	}

	// Handle multi-line data
	lines := strings.Split(event.Data, "\n")
	for _, line := range lines {
		if err := sw.writeField("data", line); err != nil {
			return err
		}
	}

	_, err := fmt.Fprint(sw.w, "\n") // End of event
	if err != nil {
		return err
	}
	sw.flusher.Flush()
	return nil
}

// SendComment sends an SSE comment (keep-alive).
func (sw *SSEWriter) SendComment(comment string) error {
	_, err := fmt.Fprintf(sw.w, ": %s\n\n", comment) // Comments start with ':' and need double newline
	if err != nil {
		return err
	}
	sw.flusher.Flush()
	return nil
}

// SendRawData is a convenience function to send raw NATS message data.
// It uses the NATS subject as the SSE event name and base64 encodes the data.
// An optional NATS message ID (e.g., from a header) can be provided.
func (sw *SSEWriter) SendRawNATSMessage(natsSubject, natsMsgID string, data []byte) error {
	encodedData := base64.StdEncoding.EncodeToString(data)
	return sw.SendEvent(SSEEvent{
		ID:    natsMsgID,
		Event: natsSubject, // Use NATS subject as SSE event type
		Data:  encodedData,
	})
}
