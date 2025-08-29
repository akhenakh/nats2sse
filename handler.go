package nats2sse

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// SubjectFunc is executing right after the HTTP request starts
// and returns the NATS subject(s) to subscribe to, and an optional time
// from which to receive historical messages.
type SubjectFunc func(r *http.Request) (subject string, clientID string, since *time.Time, err error)

// ChannelMessage is a wrapper for messages passed through the internal channel.
type ChannelMessage struct {
	NatsMsg *nats.Msg
	JsMsg   jetstream.Msg // Populated for JetStream messages, for Ack
}

// MessageReceivedCallback is a function type that will be called for every NATS message received
// before it's sent to the SSE client.
// It receives the clientID, the NATS subject, the raw NATS message data, and the NATS message headers.
// It can return a modified []byte payload to be sent to the client.
// Returning a nil []byte will cause the message to be skipped (not sent to the client).
// Returning an error will also cause the message not to be sent, and the error will be logged.
type MessageReceivedCallback func(ctx context.Context, clientID, subject string, headers nats.Header, msgData []byte) ([]byte, error)

// Handler is an http.Handler for bridging NATS JetStream messages to SSE.
type Handler struct {
	natsConn                      *nats.Conn
	subjectFunc                   SubjectFunc
	jetStreamName                 string // Optional: For JetStream, specify stream name. If empty, will try to discover.
	heartbeat                     time.Duration
	logger                        *slog.Logger
	jetStreamConsumerConfigurator func(config *jetstream.ConsumerConfig, subject string, clientID string)
	messageCallback               MessageReceivedCallback
}

// HandlerOption is a function type for configuring NATS2SSEHandler
type HandlerOption func(*Handler)

// WithNATSConnection sets the NATS connection
func WithNATSConnection(conn *nats.Conn) HandlerOption {
	return func(h *Handler) {
		h.natsConn = conn
	}
}

// WithSubjectFunc sets the subject function
func WithSubjectFunc(fn SubjectFunc) HandlerOption {
	return func(h *Handler) {
		h.subjectFunc = fn
	}
}

// WithJetStreamName sets the JetStream name
func WithJetStreamName(name string) HandlerOption {
	return func(h *Handler) {
		h.jetStreamName = name
	}
}

// WithHeartbeat sets the heartbeat duration
func WithHeartbeat(duration time.Duration) HandlerOption {
	return func(h *Handler) {
		h.heartbeat = duration
	}
}

// WithLogger sets the logger
func WithLogger(logger *slog.Logger) HandlerOption {
	return func(h *Handler) {
		h.logger = logger
	}
}

// WithJetStreamConsumerConfigurator sets the JetStream consumer configurator
func WithJetStreamConsumerConfigurator(configurator func(config *jetstream.ConsumerConfig, subject string, clientID string)) HandlerOption {
	return func(h *Handler) {
		h.jetStreamConsumerConfigurator = configurator
	}
}

// WithMessageCallback sets the message callback
func WithMessageCallback(callback MessageReceivedCallback) HandlerOption {
	return func(h *Handler) {
		h.messageCallback = callback
	}
}

// NewHandler creates a new Handler with the given options
func NewHandler(options ...HandlerOption) *Handler {
	handler := &Handler{}

	for _, option := range options {
		option(handler)
	}

	return handler
}

func (h *Handler) ensureLogger() *slog.Logger {
	if h.logger == nil {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return h.logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := h.ensureLogger()

	if h.natsConn == nil {
		logger.Error("NATS connection is not configured")
		http.Error(w, "Internal Server Error: NATS connection not configured", http.StatusInternalServerError)
		return
	}
	if h.subjectFunc == nil {
		logger.Error("SubjectFunc is not configured")
		http.Error(w, "Internal Server Error: Authentication function not configured", http.StatusInternalServerError)
		return
	}

	subject, clientID, since, err := h.subjectFunc(r)
	if err != nil {
		logger.Warn("Authentication failed", "clientID", clientID, "error", err)
		http.Error(w, "Unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if subject == "" {
		logger.Warn("Authentication returned empty subject", "clientID", clientID)
		http.Error(w, "Forbidden: No subject authorized", http.StatusForbidden)
		return
	}

	// Create a logger with client-specific context
	logger = logger.With("clientID", clientID, "subject", subject)
	logger.Info("Client authenticated")

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		logger.Error("Failed to create SSEWriter", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	msgChan := make(chan ChannelMessage, 64)

	var sub *jetStreamSubscription
	defer func() {
		if sub != nil {
			logger.Info("Unsubscribing client")
			if errUnsub := sub.Unsubscribe(); errUnsub != nil {
				logger.Error("Error during unsubscription", "error", errUnsub)
			}
		}
	}()

	js, errJs := jetstream.New(h.natsConn)
	if errJs != nil {
		logger.Error("Failed to create JetStream context", "error", errJs)
		http.Error(w, "NATS JetStream unavailable", http.StatusServiceUnavailable)
		return
	}

	streamName := h.jetStreamName
	if streamName == "" {
		discoveredStreamName, errDiscover := js.StreamNameBySubject(ctx, subject)
		if errDiscover != nil {
			logger.Error("Failed to find JetStream stream by subject", "error", errDiscover)
			errMsg := "NATS JetStream: Error finding stream for subject"
			if errors.Is(errDiscover, jetstream.ErrStreamNotFound) {
				errMsg = "NATS JetStream: No stream found for subject " + subject
			}
			http.Error(w, errMsg, http.StatusServiceUnavailable)
			return
		}
		streamName = discoveredStreamName
		logger.Debug("Discovered JetStream stream for subject", "streamName", streamName)
	}
	logger = logger.With("streamName", streamName)

	stream, errStream := js.Stream(ctx, streamName)
	if errStream != nil {
		logger.Error("Failed to get JetStream stream", "error", errStream)
		errMsg := "NATS JetStream stream " + streamName + " unavailable"
		if errors.Is(errStream, jetstream.ErrStreamNotFound) {
			errMsg = "NATS JetStream stream " + streamName + " not found"
		}
		http.Error(w, errMsg, http.StatusServiceUnavailable)
		return
	}

	consumerConfig := jetstream.ConsumerConfig{
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverNewPolicy,
	}

	if since != nil && !since.IsZero() {
		consumerConfig.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		consumerConfig.OptStartTime = since
		logger.Info("Consumer configured to deliver messages from a start time", "since", since.Format(time.RFC3339))
	}

	if h.jetStreamConsumerConfigurator != nil {
		h.jetStreamConsumerConfigurator(&consumerConfig, subject, clientID)
	}

	consumer, errConsumer := stream.CreateOrUpdateConsumer(ctx, consumerConfig)
	if errConsumer != nil {
		logger.Error("Failed to create/update JetStream consumer", slog.Any("config", consumerConfig), "error", errConsumer)
		http.Error(w, "Failed to create/update NATS JetStream consumer", http.StatusServiceUnavailable)
		return
	}
	isEphemeral := consumerConfig.Durable == ""
	consumerName := consumer.CachedInfo().Name
	logger = logger.With("consumerName", consumerName)

	jsMsgHandler := func(m jetstream.Msg) {
		// Extract NATS headers from JetStream message headers
		natsHeaders := make(nats.Header)
		for key, values := range m.Headers() {
			for _, value := range values {
				natsHeaders.Add(key, value)
			}
		}

		cm := ChannelMessage{
			NatsMsg: &nats.Msg{
				Subject: m.Subject(),
				Data:    m.Data(),
				Header:  natsHeaders,
			},
			JsMsg: m,
		}
		select {
		case msgChan <- cm:
		case <-ctx.Done():
			logger.Debug("Client disconnected, dropping JetStream message", "msgSubject", m.Subject())
			return
		}
	}

	consumeCtx, errConsume := consumer.Consume(jsMsgHandler)
	if errConsume != nil {
		logger.Error("Failed to start JetStream consumer", "error", errConsume)
		if isEphemeral && consumerName != "" {
			deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer deleteCancel()
			if delErr := js.DeleteConsumer(deleteCtx, streamName, consumerName); delErr != nil {
				if !errors.Is(delErr, jetstream.ErrConsumerNotFound) {
					logger.Error("Failed to delete ephemeral consumer after Consume() failed", "error", delErr)
				}
			}
		}
		http.Error(w, "Failed to start NATS JetStream consumer", http.StatusServiceUnavailable)
		return
	}

	sub = &jetStreamSubscription{
		js:           js,
		streamName:   streamName,
		consumerName: consumerName,
		msgCtx:       consumeCtx,
		isEphemeral:  isEphemeral,
		logger:       logger,
	}
	logger.Info("Client subscribed to JetStream")

	_ = sseWriter.SendComment("connection established, listening for events on " + subject)

	var heartbeatTicker *time.Ticker
	if h.heartbeat > 0 {
		heartbeatTicker = time.NewTicker(h.heartbeat)
		defer heartbeatTicker.Stop()
	} else {
		heartbeatTicker = &time.Ticker{C: make(<-chan time.Time)}
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("Client disconnected, terminating loop")
			return
		case chMsg, ok := <-msgChan:
			if !ok {
				logger.Warn("Main message channel closed, terminating loop")
				return
			}
			natsMsgToProcess := chMsg.NatsMsg
			msgLogger := logger.With("msgSubject", natsMsgToProcess.Subject)
			dataToSend := natsMsgToProcess.Data

			if h.messageCallback != nil {
				var cbErr error
				dataToSend, cbErr = h.messageCallback(ctx, clientID, natsMsgToProcess.Subject, natsMsgToProcess.Header, natsMsgToProcess.Data)
				if cbErr != nil {
					msgLogger.Warn("Message callback returned an error, skipping message", "error", cbErr)
					if chMsg.JsMsg != nil {
						if errAck := chMsg.JsMsg.Ack(); errAck != nil {
							msgLogger.Error("Failed to Ack JetStream message after callback failure", "error", errAck)
						}
					}
					continue
				}
				if dataToSend == nil {
					// Message filtered out by callback, no need to log verbosely.
					if chMsg.JsMsg != nil {
						if errAck := chMsg.JsMsg.Ack(); errAck != nil {
							msgLogger.Error("Failed to Ack JetStream message after callback filter", "error", errAck)
						}
					}
					continue
				}
			}

			if chMsg.JsMsg != nil {
				if errAck := chMsg.JsMsg.Ack(); errAck != nil {
					// Don't return, still try to send the message. Ack is best-effort here.
					msgLogger.Warn("Failed to Ack JetStream message", "error", errAck)
				}
			}

			natsMsgID := natsMsgToProcess.Header.Get(nats.MsgIdHdr)
			errSend := sseWriter.SendRawNATSMessage(natsMsgToProcess.Subject, natsMsgID, dataToSend)
			if errSend != nil {
				logger.Error("Failed to send SSE data, terminating", "error", errSend)
				return
			}
		case <-heartbeatTicker.C:
			errHb := sseWriter.SendComment("keep-alive")
			if errHb != nil {
				logger.Error("Failed to send SSE heartbeat, terminating", "error", errHb)
				return
			}
		}
	}
}

type jetStreamSubscription struct {
	js           jetstream.JetStream
	streamName   string
	consumerName string
	msgCtx       jetstream.ConsumeContext
	isEphemeral  bool
	logger       *slog.Logger
}

func (s *jetStreamSubscription) Unsubscribe() error {
	var firstErr error
	logger := s.logger

	if s.msgCtx != nil {
		logger.Debug("Stopping JetStream message context")
		s.msgCtx.Stop()
		s.msgCtx = nil
	}

	if s.isEphemeral && s.consumerName != "" && s.js != nil {
		logger.Info("Deleting ephemeral JetStream consumer")
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer deleteCancel()
		err := s.js.DeleteConsumer(deleteCtx, s.streamName, s.consumerName)
		if err != nil {
			if errors.Is(err, jetstream.ErrConsumerNotFound) {
				logger.Debug("Ephemeral consumer not found during deletion, likely already removed")
			} else {
				logger.Error("Failed to delete ephemeral JetStream consumer", "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	} else if s.isEphemeral {
		logger.Warn("Cannot delete ephemeral consumer, missing info", "jetstreamAvailable", s.js != nil)
	}

	return firstErr
}
