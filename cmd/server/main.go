package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/akhenakh/nats2sse"
)

// SimpleAuth is a basic authentication function.
// In a real app, you'd inspect r.Header.Get("Authorization") for a JWT, etc.
// It expects a query parameter "token" which dictates the subject.
// Example: /events?token=user123.updates -> subscribes to "user123.updates"
// Example: /events?token=jwt_token_here (decode JWT to get subject)
func SimpleAuth(r *http.Request) (subject string, clientID string, err error) {
	token := r.URL.Query().Get("token")
	if token == "" {
		return "", "", errors.New("missing token parameter")
	}

	// This is a placeholder. A real app would validate the token (e.g., JWT)
	// and derive the subject and clientID from it.
	// For this example, we'll use the token itself as the subject,
	// and a prefix of the token as clientID.
	// Ensure the subject is safe and doesn't allow arbitrary subscriptions.
	// e.g. if token = "user123.data", subject = "user123.data"
	// e.g. if token = "jwt...", decode jwt, get userID, subject = "users." + userID + ".events"

	if !isValidSubject(token) { // Basic validation
		return "", token, errors.New("invalid subject format in token")
	}

	// Use part of the token or a request ID as clientID for logging
	cid := r.Header.Get("X-Request-ID")
	if cid == "" {
		cid = "client_" + strings.Split(token, ".")[0] // Simplistic client ID
	}

	return token, cid, nil
}

// isValidSubject is a very basic validation.
// NATS subjects are case-sensitive and cannot contain spaces.
// Wildcards '>' and '*' have special meanings.
func isValidSubject(subject string) bool {
	if subject == "" || strings.ContainsAny(subject, " \t\r\n") {
		return false
	}
	// Add more robust validation as needed, e.g., prevent overly broad wildcards
	// if subject == ">" || subject == "*" { return false; }
	return true
}

func main() {
	natsURL := flag.String("nats_url", nats.DefaultURL, "NATS server URL")
	useJetStream := flag.Bool("jetstream", false, "Use NATS JetStream")
	streamName := flag.String("stream", "MY_EVENTS", "JetStream stream name (if using JetStream)")
	streamSubjects := flag.String("subjects", "events.>", "JetStream stream subjects (e.g., 'events.>')")
	httpAddr := flag.String("http_addr", ":8080", "HTTP listen address")
	flag.Parse()

	logger := log.New(os.Stdout, "NATS2SSE_APP: ", log.LstdFlags|log.Lmicroseconds)

	// --- Connect to NATS ---
	nc, err := nats.Connect(*natsURL,
		nats.Name("NATS2SSE Bridge"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(5),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			logger.Printf("NATS disconnected: %v", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Printf("NATS reconnected to %s", nc.ConnectedUrl())
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			logger.Println("NATS connection closed.")
		}),
	)
	if err != nil {
		logger.Fatalf("Error connecting to NATS: %v", err)
	}
	defer nc.Drain()
	logger.Printf("Connected to NATS server: %s", nc.ConnectedUrl())

	// Setup JetStream Stream if enabled (optional, for testing)
	if *useJetStream {
		js, err := jetstream.New(nc)
		if err != nil {
			logger.Fatalf("Error creating JetStream context: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err = js.CreateStream(ctx, jetstream.StreamConfig{
			Name:     *streamName,
			Subjects: []string{*streamSubjects}, // e.g., "events.>", "alerts.critical"
			Storage:  jetstream.MemoryStorage,   // For example, use FileStorage for persistence
		})
		if err != nil {
			// If stream already exists, that's often fine
			if !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) && !strings.Contains(err.Error(), "stream name already in use") { // Workaround for slightly different error types/messages
				logger.Fatalf("Error creating/updating JetStream stream %s: %v", *streamName, err)
			} else {
				logger.Printf("JetStream stream %s already exists or configured.", *streamName)
			}
		} else {
			logger.Printf("JetStream stream %s created/updated with subjects [%s]", *streamName, *streamSubjects)
		}

		// Example publisher for JetStream
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			i := 0
			for range ticker.C {
				// Example subjects that would match "events.>"
				subj := fmt.Sprintf("events.typeA.%d", i)
				payload := fmt.Sprintf("JS Hello %d from %s", i, subj)
				_, pubErr := js.Publish(context.Background(), subj, []byte(payload))
				if pubErr != nil {
					logger.Printf("Error publishing to JetStream subject %s: %v", subj, pubErr)
				} else {
					logger.Printf("Published to JetStream: [%s] '%s'", subj, payload)
				}
				i++
			}
		}()
	} else {
		// Example publisher for Core NATS
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			i := 0
			for range ticker.C {
				// Use a subject that SimpleAuth can authorize via token
				subj := "public.messages"
				payload := fmt.Sprintf("Core NATS Hello %d", i)
				if pubErr := nc.Publish(subj, []byte(payload)); pubErr != nil {
					logger.Printf("Error publishing to NATS subject %s: %v", subj, pubErr)
				} else {
					logger.Printf("Published to Core NATS: [%s] '%s'", subj, payload)
				}
				i++
			}
		}()
	}

	// Configure NATS to SSE Handler
	natsSSEHandler := &nats2sse.NATS2SSEHandler{
		NATSConn:      nc,
		Auth:          SimpleAuth,
		IsJetStream:   *useJetStream,
		JetStreamName: *streamName, // Important if IsJetStream is true and you want to bind
		Heartbeat:     15 * time.Second,
		Logger:        logger,
		// Example of custom JetStream Consumer Options:
		// JetStreamConsumerOpts: []jetstream.ConsumerOpt{
		// 	jetstream.DeliverAll(), // Get all messages from the stream
		// 	jetstream.AckExplicit(),
		//  // For a shared durable consumer (clientID from AuthFunc must be stable and unique):
		//  // jetstream.Durable(clientID), // clientID would need to be passed or determined
		// },
	}

	mux := http.NewServeMux()
	mux.Handle("/events", natsSSEHandler) // All requests to /events go to our handler

	// Simple root handler
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "NATS to SSE Bridge. Use /events?token=<subject_to_subscribe>")
		fmt.Fprintln(w, "Example (Core NATS): /events?token=public.messages")
		fmt.Fprintln(w, "Example (JetStream, if stream 'MY_EVENTS' has 'events.>'): /events?token=events.typeA.123")
	})

	// Start HTTP Server
	server := &http.Server{
		Addr:    *httpAddr,
		Handler: mux,
	}

	go func() {
		logger.Printf("Starting HTTP server on %s", *httpAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("HTTP server ListenAndServe: %v", err)
		}
	}()

	// Graceful Shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Fatalf("Server forced to shutdown: %v", err)
	}

	logger.Println("Server exiting")
}
