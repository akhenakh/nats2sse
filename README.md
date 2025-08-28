# NATS to SSE Bridge (nats2sse)

A Go library providing an `http.Handler` to bridge messages from NATS JetStream to Server-Sent Events (SSE) clients.

## Status

This was generated 90% using Gemini, it is useful to me use it at your own risk ;)

## Features

*   **NATS JetStream Native**: Built exclusively for NATS JetStream, supporting ephemeral or durable consumers.
*   **Historical Message Replay**: Clients can request messages from a specific point in time using a `since` timestamp parameter.
*   **Customizable Authentication**: Secure your endpoint and determine subject subscriptions per request via a flexible `AuthFunc`.
*   **On-the-fly Message Transformation**: A powerful callback function allows you to inspect, modify, filter, or reject any message before it's sent to the client.
*   **Configurable SSE Heartbeats**: Keep connections alive through proxies and firewalls.
*   **Flexible Consumer Configuration**: Fine-tune the underlying JetStream consumer (e.g., for durable consumers, replay policies, rate limits).
*   **Custom Logging**: Integrates with your existing logging setup.

## Installation

```bash
go get github.com/akhenakh/nats2sse
```
## Usage

There is an example server in `cmd/server`.

### 1. Define an Authentication Function

The `AuthFunc` is responsible for authenticating the HTTP request, determining which NATS subject the client should subscribe to, and optionally parsing a `since` parameter for historical message replay.

```go
import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// SimpleAuth demonstrates token auth, subject/clientID extraction, and parsing the 'since' parameter.
// In a real application, you'd implement proper token validation, ACL checks, etc.
func SimpleAuth(r *http.Request) (subject string, clientID string, since *time.Time, err error) {
	token := r.URL.Query().Get("token")
	subj := r.URL.Query().Get("subject")
	cID := r.URL.Query().Get("clientID") // Optional client ID for durable consumers
	sinceStr := r.URL.Query().Get("since")

	if token != "supersecret" { // Replace with real auth logic, or relly on existing solution like JWT
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
	"log"
	"net/http"
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
		log.Fatalf("Error connecting to NATS: %v", err)
	}
	defer nc.Close()

	// 2. Get JetStream context and ensure your stream exists
	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatalf("Error creating JetStream context: %v", err)
	}
	streamCfg := jetstream.StreamConfig{
		Name:     "mystream",
		Subjects: []string{"events.>"},
	}
	_, err = js.CreateOrUpdateStream(context.Background(), streamCfg)
	if err != nil {
		log.Fatalf("Error creating/updating stream: %v", err)
	}

	// 3. Create the NATS2SSE handler
	sseHandler := &nats2sse.NATS2SSEHandler{
		NATSConn:      nc,
		Auth:          SimpleAuth,
		JetStreamName: "mystream", // Specify the stream name
		Heartbeat:     30 * time.Second,
		Logger:        log.Default(),
		JetStreamConsumerConfigurator: func(config *jetstream.ConsumerConfig, subject string, clientID string) {
			// Example: Make consumer durable if clientID is provided
			if clientID != "" && strings.HasPrefix(clientID, "durable-") {
				config.Durable = clientID
				// For durables, you might want to resume from where they left off
				config.DeliverPolicy = jetstream.DeliverLastPolicy
				config.Name = clientID
			}
		},
		MessageCallback: func(ctx context.Context, clientID, subject string, headers nats.Header, msgData []byte) ([]byte, error) {
			// Example: Add a prefix to every message payload
			log.Printf("Callback received message for client %s on subject %s", clientID, subject)

			// To filter out a message, return (nil, nil)
			if string(msgData) == "skip" {
				return nil, nil
			}

			// To signal an error and drop the message, return (nil, error)
			if string(msgData) == "error" {
				return nil, errors.New("encountered an error message")
			}

			// To modify the message, return the new payload
			newData := []byte("processed: " + string(msgData))
			return newData, nil
		},
	}

	http.Handle("/events", sseHandler)
	log.Println("SSE server listening on :8080/events")
	log.Println("Try: curl -N 'http://localhost:8080/events?token=supersecret&subject=events.foo'")
	log.Println("Or with history: curl -N 'http://localhost:8080/events?token=supersecret&subject=events.foo&since=1672531200'")
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
        const subject = "events.foo"; // The NATS subject to listen to
        const token = "supersecret";
        const clientID = "web-client-123"; // Optional, for durable consumers

        // Optional: Get all messages since a specific time
        const sinceTimestamp = "1698295200"; // Unix epoch in seconds (e.g., for 2023-10-26T10:00:00Z)

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
                const decodedData = atob(event.data); // Assuming data was base64 encoded
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
*   `JetStreamName`: This field should specify the name of the JetStream stream. If left empty, the handler will attempt to discover the stream name based on the subject provided during authentication.
*   **Consumers**:
    *   By default, an **ephemeral** JetStream consumer is created for each connection with `AckExplicit` and `DeliverNew` policies. These consumers are automatically deleted when the client disconnects.
    *   You can create **durable** consumers by providing a `clientID` in the `AuthFunc` and using the `JetStreamConsumerConfigurator` to set the `Durable` field in the consumer config.
*   **Historical Replay (`since` parameter)**:
    *   When a client provides a valid `since` timestamp (as a string representing seconds since the Unix epoch), the handler automatically configures the underlying JetStream consumer with `DeliverPolicy: DeliverByStartTimePolicy`.
    *   This instructs JetStream to send all messages stored in the stream that are newer than the specified time, before switching to live messages. This is a powerful feature for clients that need to catch up on missed events.
*   `MessageCallback`: This function is called for every message *before* it is sent to the client.
    *   Return `(data, nil)` to send the (potentially modified) data.
    *   Return `(nil, nil)` to silently filter/drop the message.
    *   Return `(nil, error)` to drop the message and log the error.

## Error Handling

*   The handler logs errors to the configured `Logger` (or `log.Default()`).
*   Authentication failures result in HTTP `401 Unauthorized` or `403 Forbidden`.
*   NATS connection or subscription issues typically result in HTTP `500 Internal Server Error` or `503 Service Unavailable`.
