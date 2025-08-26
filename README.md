# NATS to SSE Bridge (nats2sse)

A Go library providing an `http.Handler` to bridge messages from NATS (including JetStream) to Server-Sent Events (SSE) clients.


## Status

This was generated 99% using Gemini, it is useful to me use it at your own risk ;)


## Features

*   Supports NATS Core subscriptions.
*   Supports NATS JetStream subscriptions (ephemeral or durable consumers).
*   Customizable authentication via `AuthFunc`.
*   Configurable SSE heartbeats for keep-alive.
*   Optional logger.
*   JetStream consumer configuration can be customized.
*   Callbacks for message processing and error handling.

## Installation

```bash
go get github.com/akhenakh/nats2sse
```

## Usage

There is an example server in `cmd/server`.

### 1. Define an Authentication Function

The `AuthFunc` is responsible for authenticating the HTTP request and determining which NATS subject(s) the client should subscribe to. It can also return a `clientID` for logging or creating durable JetStream consumers.

```go
import (
	"net/http"
	"errors"
)

// SimpleAuth allows any client to subscribe to a subject provided in a query parameter.
// In a real application, you'd implement proper token validation, ACL checks, JWT validation, etc.
func SimpleAuth(r *http.Request) (subject string, clientID string, err error) {
	token := r.URL.Query().Get("token")
	subj := r.URL.Query().Get("subject")
	cID := r.URL.Query().Get("clientID") // Optional client ID

	if token != "supersecret" { // Replace with real auth logic
		return "", "", errors.New("invalid token")
	}
	if subj == "" {
		return "", "", errors.New("subject parameter is required")
	}
	if cID == "" {
		cID = "anonymous-" + subj // Or generate a unique one
	}
	return subj, cID, nil
}
```

### 2. Initialize and Use the Handler

```go
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream" // If using JetStream
	"github.com/akhenakh/nats2sse"
)

// Example SimpleAuth (from above)
func SimpleAuth(r *http.Request) (subject string, clientID string, err error) {
	// ... (implementation from above)
	token := r.URL.Query().Get("token")
	subj := r.URL.Query().Get("subject")
	cID := r.URL.Query().Get("clientID")

	if token != "supersecret" {
		return "", "", errors.New("invalid token")
	}
	if subj == "" {
		return "", "", errors.New("subject parameter is required")
	}
	if cID == "" {
		cID = "anonymous-" + subj
	}
	return subj, cID, nil
}


func main() {
	// Connect to NATS
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		log.Fatalf("Error connecting to NATS: %v", err)
	}
	defer nc.Close()

	// --- For JetStream (Optional) ---
	// js, err := jetstream.New(nc)
	// if err != nil {
	// 	log.Fatalf("Error creating JetStream context: %v", err)
	// }
	// // Ensure stream exists
	// streamCfg := jetstream.StreamConfig{
	// 	Name:     "mystream",
	// 	Subjects: []string{"events.>"},
	// }
	// _, err = js.CreateOrUpdateStream(context.Background(), streamCfg)
	// if err != nil {
	// 	log.Fatalf("Error creating/updating stream: %v", err)
	// }
	// ---------------------------------

	// Create the NATS2SSE handler
	sseHandler := &nats2sse.NATS2SSEHandler{
		NATSConn: nc,
		Auth:     SimpleAuth,
		// IsJetStream:   true,          // Set to true if using JetStream
		// JetStreamName: "mystream",    // Required if IsJetStream is true and stream name isn't discovered by subject
		Heartbeat: 30 * time.Second,
		Logger:    log.Default(),
		// JetStreamConsumerConfigurator: func(config *jetstream.ConsumerConfig, subject string, clientID string) {
		// 	// Example: Make consumer durable if clientID is provided
		// 	if clientID != "" && strings.HasPrefix(clientID, "durable-") {
		// 		config.Durable = clientID
		// 		config.DeliverPolicy = jetstream.DeliverAllPolicy // For durables, often want all messages
		//      config.Name = clientID // Explicitly set consumer name
		// 	}
		// },
	}

	http.Handle("/events", sseHandler)
	log.Println("SSE server listening on :8080/events")
	log.Println("Try: curl -N 'http://localhost:8080/events?token=supersecret&subject=foo.bar'")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("HTTP server error: %v", err)
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
        const subject = "foo.bar"; // Or get from user input
        const token = "supersecret";
        const clientID = "web-client-123"; // Optional

        // Construct the URL, ensuring parameters are URL-encoded
        const params = new URLSearchParams();
        params.append("token", token);
        params.append("subject", subject);
        if (clientID) {
            params.append("clientID", clientID);
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

        // Generic message handler (if specific event names are not used often from NATS subjects)
        // eventSource.onmessage = function(event) {
        //     const li = document.createElement("li");
        //     // Data is base64 encoded by the server
        //     const decodedData = atob(event.data);
        //     li.textContent = `Event: ${event.type}, ID: ${event.id || 'N/A'}, Data: ${decodedData}`;
        //     document.getElementById("messages").appendChild(li);
        //     console.log("Generic message:", event);
        // };

        // Listen for specific events (NATS subjects)
        // Replace "foo.bar" with the actual subject you are subscribing to
        eventSource.addEventListener(subject, function(event) {
            const li = document.createElement("li");
            // Data is base64 encoded by the server
            try {
                const decodedData = atob(event.data); // Assuming data was base64 encoded
                li.textContent = `Event Type (Subject): ${event.type}, ID: ${event.id || 'N/A'}, Data: ${decodedData}`;
                console.log("Specific event:", event.type, "Data:", decodedData);
            } catch (e) {
                li.textContent = `Event Type (Subject): ${event.type}, ID: ${event.id || 'N/A'}, Raw Data: ${event.data} (Error decoding: ${e})`;
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
            eventSource.close(); // Close on error to prevent constant retries from browser if server is down
        };

        // Optional: Handle comments (keep-alives)
        eventSource.addEventListener('comment', function(event) {
            console.log("Received comment:", event.data);
        });
    </script>
</body>
</html>
```

## JetStream Considerations

*   If `IsJetStream` is `true`:
    *   The `JetStreamName` field should specify the name of the JetStream stream to bind to. If left empty, the handler will attempt to discover the stream name using `js.StreamNameBySubject()`.
    *   By default, an ephemeral JetStream consumer is created with `AckExplicit` and `DeliverNew` policies.
    *   You can customize the `jetstream.ConsumerConfig` using the `JetStreamConsumerConfigurator` function. This is useful for creating durable consumers, setting different deliver policies, rate limits, etc.
    *   Make sure the stream and its subjects are correctly configured in your NATS server.
    *   Ephemeral consumers are automatically deleted when the SSE client disconnects. Durable consumers persist.

## Error Handling

*   The handler logs errors to the configured `Logger` (or `log.Default()`).
*   Authentication failures result in HTTP `401 Unauthorized` or `403 Forbidden`.
*   NATS connection or subscription issues typically result in HTTP `500 Internal Server Error` or `503 Service Unavailable`.
