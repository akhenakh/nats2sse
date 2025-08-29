# NATS to SSE Bridge (nats2sse)

A Go library providing an `http.Handler` to bridge messages from NATS JetStream to Server-Sent Events (SSE) clients.

## Status

This was generated 90% using Gemini, it is useful to me use it at your own risk ;)

## Features

*   **NATS JetStream Native**: Built exclusively for NATS JetStream, supporting ephemeral or durable consumers.
*   **Historical Message Replay**: Clients can request messages from a specific point in time using a `since` timestamp parameter.
*   **Customizable Authentication & Authorization**: Secure your endpoint and determine subject subscriptions per request via a flexible `SubjectFunc`.
*   **On-the-fly Message Transformation**: A powerful callback function allows you to inspect, modify, filter, or reject any message before it's sent to the client.
*   **Configurable SSE Heartbeats**: Keep connections alive through proxies and firewalls.
*   **Flexible Consumer Configuration**: Fine-tune the underlying JetStream consumer (e.g., for durable consumers, replay policies, rate limits).

## Installation

```bash
go get github.com/akhenakh/nats2sse
```
## Usage

There is an example server in `cmd/server`.

### 1. Define a Subject Function

The `SubjectFunc` is responsible to validate the HTTP request, determining which NATS subject the client should subscribe to, and optionally parsing a `since` parameter for historical message replay.

```go
import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// SimpleAuth demonstrates infering the proper subject, token auth, clientID extraction, and parsing the 'since' parameter.
// In a real application, you'd implement proper token validation (e.g., JWT), ACL checks, etc.
func SimpleAuth(r *http.Request) (subject string, clientID string, since *time.Time, err error) {
	token := r.URL.Query().Get("token")
	subj := r.URL.Query().Get("subject")
	cID := r.URL.Query().Get("clientID") // Optional client ID for durable consumers
	sinceStr := r.URL.Query().Get("since")

	if token != "supersecret" { // Replace with real auth logic
		return "", "", nil, errors.New("invalid token")
	}
	if subj == "" {
		return "", "", nil, errors.New("subject parameter is required")
	}
	if cID == "" {
		cID = "anonymous-" + subj // Or generate a unique one for ephemeral consumers
	}

	// Handle optional 'since' parameter for historical messages
	if sinceStr != "" {
		epoch, errParse := strconv.ParseInt(sinceStr, 10, 64)
		if errParse != nil {
			return "", "", nil, fmt.Errorf("invalid 'since' parameter format (requires Unix epoch seconds): %w", errParse)
		}
		t := time.Unix(epoch, 0)
		since = &t
	}

	return subj, cID, since, nil
}
```

### 2. Initialize and Use the Handler

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akhenakh/nats2sse"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// SimpleAuth implementation from above...

func main() {
	// 1. Connect to NATS
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		slog.Error("Error connecting to NATS", "error", err)
		os.Exit(1)
	}
	defer nc.Close()

	// 2. Get JetStream context and ensure your stream exists
	js, err := jetstream.New(nc)
	if err != nil {
		slog.Error("Error creating JetStream context", "error", err)
		os.Exit(1)
	}
	streamCfg := jetstream.StreamConfig{
		Name:     "mystream",
		Subjects: []string{"events.>"},
	}
	_, err = js.CreateOrUpdateStream(context.Background(), streamCfg)
	if err != nil {
		slog.Error("Error creating/updating stream", "error", err)
		os.Exit(1)
	}

	// 3. Create the nats2sse handler using functional options
	sseHandler := nats2sse.NewHandler(
		nats2sse.WithNATSConnection(nc),
		nats2sse.WithSubjectFunc(SimpleAuth),
		nats2sse.WithJetStreamName("mystream"), // Specify the stream name
		nats2sse.WithHeartbeat(30*time.Second),
		nats2sse.WithLogger(slog.Default()),
		nats2sse.WithJetStreamConsumerConfigurator(func(config *jetstream.ConsumerConfig, subject string, clientID string) {
			// Example: Make consumer durable if clientID is provided
			if clientID != "" && strings.HasPrefix(clientID, "durable-") {
				config.Durable = clientID
				// For durables, you might want to resume from where they left off
				config.DeliverPolicy = jetstream.DeliverLastPolicy
			}
		}),
		nats2sse.WithMessageCallback(func(ctx context.Context, clientID, subject string, headers nats.Header, msgData []byte) ([]byte, error) {
			slog.Info("Callback received message", "clientID", clientID, "subject", subject)

			// To filter out a message, return (nil, nil)
			if string(msgData) == "skip" {
				return nil, nil
			}
			// To signal an error and drop the message, return (nil, error)
			if string(msgData) == "error" {
				return nil, errors.New("encountered an error message")
			}
			// To modify the message, return the new payload
			return []byte("processed: " + string(msgData)), nil
		}),
	)

	http.Handle("/events", sseHandler)
	slog.Info("SSE server listening", "address", "http://localhost:8080/events")
	slog.Info("Try: curl -N 'http://localhost:8080/events?token=supersecret&subject=events.foo'")
	slog.Info("Or with history: curl -N 'http://localhost:8080/events?token=supersecret&subject=events.foo&since=1672531200'")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		slog.Error("HTTP server error", "error", err)
		os.Exit(1)
	}
}
```

### 3. Client-Side (JavaScript Example)

```html
<!DOCTYPE html>
<html>
<head>
    <title>NATS to SSE Demo</title>
</head>
<body>
    <h1>SSE Messages:</h1>
    <ul id="messages"></ul>

    <script>
        // --- Configuration ---
        const subject = "events.foo"; // The NATS subject to listen to
        const token = "supersecret";
        const clientID = "web-client-123"; // Optional, for durable consumers

        // Optional: Get all messages since a specific time (Unix epoch in seconds).
        // Leave as null or an empty string to receive only new messages.
        const sinceTimestamp = "1698295200"; // e.g., for 2023-10-26T10:00:00Z
        // --- End Configuration ---

        // Construct the URL, ensuring parameters are URL-encoded
        const params = new URLSearchParams();
        params.append("token", token);
        params.append("subject", subject);
        if (clientID) {
            params.append("clientID", clientID);
        }
        if (sinceTimestamp) {
            params.append("since", sinceTimestamp);
        }

        const eventSourceUrl = `/events?${params.toString()}`;
        console.log("Connecting to:", eventSourceUrl);
        const eventSource = new EventSource(eventSourceUrl);

        eventSource.onopen = function() {
            console.log("SSE Connection opened.");
            const li = document.createElement("li");
            li.textContent = "SSE Connection opened.";
            document.getElementById("messages").appendChild(li);
        };

        // Listen for events whose type matches the NATS subject
        eventSource.addEventListener(subject, function(event) {
            const li = document.createElement("li");
            // Data is base64 encoded by the server
            try {
                const decodedData = atob(event.data);
                li.textContent = `Event (Subject: ${event.type}): ${decodedData}`;
                console.log("Specific event:", event.type, "Data:", decodedData);
            } catch (e) {
                li.textContent = `Event (Subject: ${event.type}), Raw Data: ${event.data} (Error decoding: ${e})`;
                console.error("Error decoding base64 data:", e, "Raw data:", event.data);
            }
            document.getElementById("messages").appendChild(li);
        });

        eventSource.onerror = function(err) {
            console.error("EventSource failed:", err);
            const li = document.createElement("li");
            li.textContent = "SSE Connection error. See console.";
            li.style.color = "red";
            document.getElementById("messages").appendChild(li);
            eventSource.close(); // Close on error to prevent constant retries from browser
        };
    </script>
</body>
</html>
```

## Handler Configuration Details

*   The handler operates exclusively with NATS JetStream. You must have a JetStream-enabled NATS server.
*   `WithJetStreamName`: This option should specify the name of the JetStream stream. If left empty, the handler will attempt to discover the stream name based on the subject provided during authentication.
*   **Consumers**:
    *   By default, an **ephemeral** JetStream consumer is created for each connection with `AckExplicit` and `DeliverNew` policies. These consumers are automatically deleted when the client disconnects.
    *   You can create **durable** consumers by providing a `clientID` in the `SubjectFunc` and using the `WithJetStreamConsumerConfigurator` option to set the `Durable` field in the consumer config.
*   **Historical Replay (`since` parameter)**:
    *   When a client provides a valid `since` timestamp (as a string representing seconds since the Unix epoch), the handler automatically configures the underlying JetStream consumer with `DeliverPolicy: DeliverByStartTimePolicy`.
    *   This instructs JetStream to send all messages stored in the stream that are newer than the specified time, before switching to live messages. This is a powerful feature for clients that need to catch up on missed events.
*   `WithMessageCallback`: This function is called for every message *before* it is sent to the client.
    *   Return `(data, nil)` to send the (potentially modified) data.
    *   Return `(nil, nil)` to silently filter/drop the message.
    *   Return `(nil, error)` to drop the message and log the error.

## Error Handling

*   The handler logs errors to the configured `slog.Logger`.
*   Authentication failures result in HTTP `401 Unauthorized` or `403 Forbidden`.
*   NATS connection or subscription issues typically result in HTTP `503 Service Unavailable`.
