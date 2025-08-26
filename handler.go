package nats2sse

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// AuthFunc is a function type that authenticates an HTTP request
// and returns the NATS subject(s) to subscribe to.
type AuthFunc func(r *http.Request) (subject string, clientID string, err error)

// ChannelMessage is a wrapper for messages passed through the internal channel.
type ChannelMessage struct {
	NatsMsg *nats.Msg
	JsMsg   jetstream.Msg // Populated only for JetStream messages, for Ack
}

// MessageReceivedCallback is a function type that will be called for every NATS message received
// before it's sent to the SSE client.
// It receives the clientID, the NATS subject, the raw NATS message data,
// and the NATS message headers.
// Returning an error will cause the message not to be sent to the SSE client for this specific connection,
// and the error will be logged.
type MessageReceivedCallback func(ctx context.Context, clientID, subject string, headers nats.Header, msgData []byte) error

// NATS2SSEHandler is an http.Handler for bridging NATS messages to SSE.
type NATS2SSEHandler struct {
	NATSConn                      *nats.Conn
	Auth                          AuthFunc
	IsJetStream                   bool
	JetStreamName                 string // Optional: For JetStream, specify stream name. If empty, will try to discover.
	Heartbeat                     time.Duration
	Logger                        *log.Logger
	JetStreamConsumerConfigurator func(config *jetstream.ConsumerConfig, subject string, clientID string)
	MessageCallback               MessageReceivedCallback // New field for the callback
}

func (h *NATS2SSEHandler) ensureLogger() *log.Logger {
	if h.Logger == nil {
		return log.Default()
	}
	return h.Logger
}

func (h *NATS2SSEHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := h.ensureLogger()

	if h.NATSConn == nil {
		logger.Println("Error: NATS2SSEHandler.NATSConn is nil")
		http.Error(w, "Internal Server Error: NATS connection not configured", http.StatusInternalServerError)
		return
	}
	if h.Auth == nil {
		logger.Println("Error: NATS2SSEHandler.Auth is nil")
		http.Error(w, "Internal Server Error: Authentication function not configured", http.StatusInternalServerError)
		return
	}

	subject, clientID, err := h.Auth(r)
	if err != nil {
		logger.Printf("Authentication failed for client %s: %v", clientID, err)
		http.Error(w, "Unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if subject == "" {
		logger.Printf("Authentication for client %s returned empty subject", clientID)
		http.Error(w, "Forbidden: No subject authorized", http.StatusForbidden)
		return
	}

	logger.Printf("Client %s authenticated for subject: %s", clientID, subject)

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		logger.Printf("Error creating SSEWriter for client %s: %v", clientID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	msgChan := make(chan ChannelMessage, 64)

	var sub NatsSubscription
	defer func() {
		if sub != nil && sub.IsValid() {
			logger.Printf("Client %s: Unsubscribing from subject %s", clientID, subject)
			if errUnsub := sub.Unsubscribe(); errUnsub != nil {
				logger.Printf("Client %s: Error during unsubscription from %s: %v", clientID, subject, errUnsub)
			}
		}
	}()

	if h.IsJetStream {
		js, errJs := jetstream.New(h.NATSConn)
		if errJs != nil {
			logger.Printf("Error creating JetStream context for client %s: %v", clientID, errJs)
			http.Error(w, "NATS JetStream unavailable", http.StatusServiceUnavailable)
			return
		}

		streamName := h.JetStreamName
		if streamName == "" {
			discoveredStreamName, errDiscover := js.StreamNameBySubject(ctx, subject)
			if errDiscover != nil {
				logger.Printf("Error finding JetStream stream for subject %s for client %s: %v", subject, clientID, errDiscover)
				errMsg := "NATS JetStream: Error finding stream for subject"
				if errors.Is(errDiscover, jetstream.ErrStreamNotFound) {
					errMsg = "NATS JetStream: No stream found for subject " + subject
				}
				http.Error(w, errMsg, http.StatusServiceUnavailable)
				return
			}
			streamName = discoveredStreamName
			logger.Printf("Client %s: Discovered JetStream stream %s for subject %s", clientID, streamName, subject)
		}

		stream, errStream := js.Stream(ctx, streamName)
		if errStream != nil {
			logger.Printf("Error getting JetStream stream %s for client %s: %v", streamName, clientID, errStream)
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

		if h.JetStreamConsumerConfigurator != nil {
			h.JetStreamConsumerConfigurator(&consumerConfig, subject, clientID)
		}

		consumer, errConsumer := stream.CreateOrUpdateConsumer(ctx, consumerConfig)
		if errConsumer != nil {
			logger.Printf("Error creating/updating JetStream consumer for subject %s on stream %s for client %s (config: %+v): %v", subject, streamName, clientID, consumerConfig, errConsumer)
			http.Error(w, "Failed to create/update NATS JetStream consumer", http.StatusServiceUnavailable)
			return
		}
		isEphemeral := consumerConfig.Durable == ""
		consumerName := consumer.CachedInfo().Name

		jsMsgHandler := func(m jetstream.Msg) {
			// Extract NATS headers from JetStream message headers
			natsHeaders := make(nats.Header)
			for key := range m.Headers() {
				natsHeaders.Set(key, m.Headers().Get(key))
			}

			cm := ChannelMessage{
				NatsMsg: &nats.Msg{
					Subject: m.Subject(),
					Data:    m.Data(),
					Header:  natsHeaders, // Use the extracted headers
				},
				JsMsg: m,
			}
			select {
			case msgChan <- cm:
			case <-ctx.Done():
				logger.Printf("JetStream consumer for %s (%s): client disconnected, not sending message from subject %s", clientID, consumerName, m.Subject())
				return
			}
		}

		consumeCtx, errConsume := consumer.Consume(jsMsgHandler)
		if errConsume != nil {
			logger.Printf("Error starting JetStream consumer %s for subject %s on stream %s for client %s: %v", consumerName, subject, streamName, clientID, errConsume)
			if isEphemeral && consumerName != "" {
				deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer deleteCancel()
				if delErr := js.DeleteConsumer(deleteCtx, streamName, consumerName); delErr != nil {
					if !errors.Is(delErr, jetstream.ErrConsumerNotFound) {
						logger.Printf("Error deleting ephemeral consumer %s on stream %s after failed Consume() attempt: %v", consumerName, streamName, delErr)
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
			clientID:     clientID,
		}
		logger.Printf("Client %s subscribed to JetStream subject: %s via consumer %s on stream %s", clientID, subject, consumerName, streamName)

	} else { // Core NATS
		natsMsgDirectChan := make(chan *nats.Msg, 64)
		coreSubHandle, errSub := h.NATSConn.ChanSubscribe(subject, natsMsgDirectChan)
		if errSub != nil {
			logger.Printf("Error subscribing to NATS subject %s for client %s: %v", subject, clientID, errSub)
			http.Error(w, "Failed to subscribe to NATS feed", http.StatusServiceUnavailable)
			return
		}
		sub = &coreNatsSubscription{sub: coreSubHandle}
		logger.Printf("Client %s subscribed to NATS subject: %s", clientID, subject)

		go func() {
			defer close(natsMsgDirectChan)
			for {
				select {
				case <-ctx.Done():
					logger.Printf("Core NATS adapter for client %s, subject %s: context done, stopping.", clientID, subject)
					return
				case natsMsg, ok := <-natsMsgDirectChan:
					if !ok {
						logger.Printf("Core NATS channel closed for client %s, subject %s. Stopping adapter.", clientID, subject)
						return
					}
					cm := ChannelMessage{NatsMsg: natsMsg, JsMsg: nil}
					select {
					case msgChan <- cm:
					case <-ctx.Done():
						logger.Printf("Core NATS adapter for client %s, subject %s: context done while sending to main chan. Stopping adapter.", clientID, subject)
						return
					}
				}
			}
		}()
	}

	_ = sseWriter.SendComment("connection established, listening for events on " + subject)

	var heartbeatTicker *time.Ticker
	if h.Heartbeat > 0 {
		heartbeatTicker = time.NewTicker(h.Heartbeat)
		defer heartbeatTicker.Stop()
	} else {
		heartbeatTicker = &time.Ticker{C: make(<-chan time.Time)}
	}

	for {
		select {
		case <-ctx.Done():
			logger.Printf("Client %s disconnected from subject %s. Main loop terminating.", clientID, subject)
			return
		case chMsg, ok := <-msgChan:
			if !ok {
				logger.Printf("Client %s: Main message channel closed for subject %s. Terminating.", clientID, subject)
				return
			}
			natsMsgToProcess := chMsg.NatsMsg

			if h.MessageCallback != nil {
				if cbErr := h.MessageCallback(ctx, clientID, natsMsgToProcess.Subject, natsMsgToProcess.Header, natsMsgToProcess.Data); cbErr != nil {
					logger.Printf("Client %s: Message callback for subject %s returned error: %v. Message will not be sent to SSE.", clientID, natsMsgToProcess.Subject, cbErr)
					if chMsg.JsMsg != nil {
						if errAck := chMsg.JsMsg.Ack(); errAck != nil {
							logger.Printf("Error Acking JetStream message for client %s on subject %s after callback failure: %v", clientID, natsMsgToProcess.Subject, errAck)
						}
					}
					continue
				}
			}

			if chMsg.JsMsg != nil {
				if errAck := chMsg.JsMsg.Ack(); errAck != nil {
					logger.Printf("Error Acking JetStream message for client %s on subject %s: %v", clientID, natsMsgToProcess.Subject, errAck)
				}
			}

			natsMsgID := natsMsgToProcess.Header.Get(nats.MsgIdHdr)
			errSend := sseWriter.SendRawNATSMessage(natsMsgToProcess.Subject, natsMsgID, natsMsgToProcess.Data)
			if errSend != nil {
				logger.Printf("Error sending SSE data for client %s on subject %s: %v. Terminating.", clientID, subject, errSend)
				return
			}
		case <-heartbeatTicker.C:
			errHb := sseWriter.SendComment("keep-alive")
			if errHb != nil {
				logger.Printf("Error sending SSE heartbeat for client %s: subject %s %v. Terminating.", clientID, subject, errHb)
				return
			}
		}
	}
}

// NatsSubscription is an interface to abstract NATS core and JetStream subscriptions
type NatsSubscription interface {
	Unsubscribe() error
	IsValid() bool
}

type coreNatsSubscription struct {
	sub *nats.Subscription
}

func (s *coreNatsSubscription) Unsubscribe() error {
	if s.sub == nil {
		return nil
	}
	err := s.sub.Unsubscribe()
	s.sub = nil
	return err
}
func (s *coreNatsSubscription) IsValid() bool {
	return s.sub != nil && s.sub.IsValid()
}

type jetStreamSubscription struct {
	js           jetstream.JetStream
	streamName   string
	consumerName string
	msgCtx       jetstream.ConsumeContext
	isEphemeral  bool
	logger       *log.Logger
	clientID     string
}

func (s *jetStreamSubscription) Unsubscribe() error {
	var firstErr error

	if s.msgCtx != nil {
		s.logger.Printf("Client %s: Stopping JetStream message context for consumer %s on stream %s", s.clientID, s.consumerName, s.streamName)
		s.msgCtx.Stop()
		s.msgCtx = nil
	}

	if s.isEphemeral && s.consumerName != "" && s.js != nil {
		s.logger.Printf("Client %s: Deleting ephemeral JetStream consumer %s on stream %s", s.clientID, s.consumerName, s.streamName)
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer deleteCancel()
		err := s.js.DeleteConsumer(deleteCtx, s.streamName, s.consumerName)
		if err != nil {
			if errors.Is(err, jetstream.ErrConsumerNotFound) {
				s.logger.Printf("Client %s: Ephemeral JetStream consumer %s on stream %s already deleted or not found.", s.clientID, s.consumerName, s.streamName)
			} else {
				s.logger.Printf("Client %s: Error deleting ephemeral JetStream consumer %s on stream %s: %v", s.clientID, s.consumerName, s.streamName, err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	} else if s.isEphemeral {
		s.logger.Printf("Client %s: Cannot delete ephemeral JetStream consumer. Name: '%s', JS available: %t", s.clientID, s.consumerName, s.js != nil)
	}

	return firstErr
}

func (s *jetStreamSubscription) IsValid() bool {
	return s.msgCtx != nil || (s.isEphemeral && s.consumerName != "" && s.js != nil)
}
