package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"embed"

	"github.com/akhenakh/nats2sse"
)

//go:embed static/index.html
var indexHTML embed.FS

// SimpleAuth is the SubjectFunc for the handler.
// It authenticates the request and determines the NATS subject and other parameters.
// In a real app, you'd inspect r.Header.Get("Authorization") for a JWT, etc.
// Example: /events?subject=user123.updates -> subscribes to "user123.updates"
// Example: /events?subject=user123.updates&since=1672531200 -> gets historical messages
func SimpleAuth(r *http.Request) (subject string, clientID string, since *time.Time, err error) {
	subjectParam := r.URL.Query().Get("subject")
	if subjectParam == "" {
		return "", "", nil, errors.New("missing subject parameter")
	}
	if !isValidSubject(subjectParam) {
		return "", "", nil, fmt.Errorf("invalid subject: %s", subjectParam)
	}

	// This is a placeholder for deriving a clientID. A real app might get it from a token.
	// We'll use the subject as a basic, non-durable clientID.
	clientID = subjectParam

	// Handle optional 'since' parameter for historical messages
	sinceStr := r.URL.Query().Get("since")
	if sinceStr != "" {
		epoch, errParse := strconv.ParseInt(sinceStr, 10, 64)
		if errParse != nil {
			return "", "", nil, fmt.Errorf("invalid 'since' parameter format (requires Unix epoch seconds): %w", errParse)
		}
		t := time.Unix(epoch, 0)
		since = &t
	}

	return subjectParam, clientID, since, nil
}

// isValidSubject is a very basic validation.
func isValidSubject(subject string) bool {
	return subject != "" && !strings.ContainsAny(subject, " \t\r\n")
}

func main() {
	natsURL := flag.String("nats_url", nats.DefaultURL, "NATS server URL")
	streamName := flag.String("stream", "EVENTS", "JetStream stream name")
	streamSubjects := flag.String("subjects", "events.>", "JetStream stream subjects (e.g., 'events.>')")
	httpAddr := flag.String("http_addr", ":8080", "HTTP listen address")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Connect to NATS
	nc, err := nats.Connect(*natsURL,
		nats.Name("NATS2SSE Bridge"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(5),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			logger.Warn("NATS disconnected", "error", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("NATS reconnected", "url", nc.ConnectedUrl())
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			logger.Info("NATS connection closed.")
		}),
	)
	if err != nil {
		logger.Error("Error connecting to NATS", "error", err)
		os.Exit(1)
	}
	defer nc.Drain()
	logger.Info("Connected to NATS server", "url", nc.ConnectedUrl())

	// Setup JetStream Stream
	js, err := jetstream.New(nc)
	if err != nil {
		logger.Error("Error creating JetStream context", "error", err)
		os.Exit(1)
	}

	ctx, cancelJsSetup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelJsSetup()

	stream, err := js.Stream(ctx, *streamName)
	if err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			logger.Info("Creating JetStream stream", "streamName", *streamName, "subjects", *streamSubjects)
			_, err = js.CreateStream(ctx, jetstream.StreamConfig{
				Name:     *streamName,
				Subjects: []string{*streamSubjects},
				Storage:  jetstream.MemoryStorage, // Use FileStorage for persistence
			})
			if err != nil {
				logger.Error("Error creating JetStream stream", "streamName", *streamName, "error", err)
				os.Exit(1)
			}
		} else {
			logger.Error("Error getting JetStream stream", "streamName", *streamName, "error", err)
			os.Exit(1)
		}
	} else {
		logger.Info("Using existing JetStream stream", "streamName", stream.CachedInfo().Config.Name)
	}

	// Example publisher for JetStream
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		i := 0
		for range ticker.C {
			subj := fmt.Sprintf("events.typeA.%d", i%5) // Cycle through a few subjects
			payload := fmt.Sprintf("JS Hello %d from %s", i, subj)
			_, pubErr := js.Publish(ctx, subj, []byte(payload))
			if pubErr != nil {
				logger.Warn("Error publishing to JetStream", "subject", subj, "error", pubErr)
			} else {
				logger.Info("Published to JetStream", "subject", subj, "payload", payload)
			}
			i++
		}
	}()

	// Configure NATS to SSE Handler using the new functional options
	sseHandler := nats2sse.NewHandler(
		nats2sse.WithNATSConnection(nc),
		nats2sse.WithSubjectFunc(SimpleAuth),
		nats2sse.WithJetStreamName(*streamName),
		nats2sse.WithHeartbeat(15*time.Second),
		nats2sse.WithLogger(logger),
		// Example of customizing the JetStream consumer for each client
		nats2sse.WithJetStreamConsumerConfigurator(func(config *jetstream.ConsumerConfig, subject string, clientID string) {
			// For example, if a clientID is passed that indicates durability:
			if strings.HasPrefix(clientID, "durable-") {
				config.Durable = clientID
				config.DeliverPolicy = jetstream.DeliverLastPolicy // Resume from last received message
			}
		}),
		// Example of processing messages before they are sent to the client
		nats2sse.WithMessageCallback(func(ctx context.Context, clientID, subject string, headers nats.Header, msgData []byte) ([]byte, error) {
			logger.Debug("Message callback invoked", "clientID", clientID, "subject", subject)
			// To filter a message, return nil data
			if bytes.Contains(msgData, []byte("internal")) {
				return nil, nil
			}
			// To modify a message, return new data
			return append([]byte("processed: "), msgData...), nil
		}),
	)

	mux := http.NewServeMux()
	mux.Handle("/events", sseHandler)
	mux.Handle("/", http.FileServer(http.FS(indexHTML)))

	// Start HTTP Server
	server := &http.Server{
		Addr:    *httpAddr,
		Handler: mux,
	}

	go func() {
		logger.Info("Starting HTTP server", "address", *httpAddr)
		logger.Info("Try opening http://localhost:8080 in your browser")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server ListenAndServe", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful Shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("Server forced to shutdown", "error", err)
	}

	logger.Info("Server exiting")
}
