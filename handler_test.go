package nats2sse

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nstest "github.com/nats-io/nats-server/v2/test" // Common alias for the test utilities
	natsclient "github.com/nats-io/nats.go"         // Alias for clarity if needed, or just use nats.go
	"github.com/nats-io/nats.go/jetstream"
)

const (
	testSubjectCore      = "test.core.subject"
	testSubjectJetStream = "test.js.subject"
	testStreamName       = "TESTSTREAM"
	testClientID         = "test-client-123"
	testToken            = "testtoken"
)

// Dummy AuthFunc for testing
func testAuthFunc(r *http.Request) (subject string, clientID string, err error) {
	token := r.URL.Query().Get("token")
	subj := r.URL.Query().Get("subject")

	if token != testToken {
		return "", "", errors.New("invalid test token")
	}
	if subj == "" {
		return "", "", errors.New("subject parameter required for test")
	}
	return subj, testClientID, nil
}

// Helper function to setup an embedded NATS server.
func setupTestServer(t *testing.T, enableJetStream bool) (natsConn *natsclient.Conn, jsCtx jetstream.JetStream, cleanupFunc func()) {
	t.Helper()

	opts := nstest.DefaultTestOptions
	opts.Port = -1 // Random port
	opts.JetStream = enableJetStream
	if enableJetStream {
		opts.StoreDir = t.TempDir() // Only need StoreDir if JetStream is enabled
	}
	// opts.Trace = true // Uncomment for detailed server tracing
	// opts.Debug = true // Uncomment for server debug logs

	srv := nstest.RunServer(&opts)

	nc, err := natsclient.Connect(srv.ClientURL(), natsclient.Timeout(5*time.Second))
	if err != nil {
		srv.Shutdown()
		t.Fatalf("Failed to connect to NATS server at %s: %v", srv.ClientURL(), err)
	}

	var js jetstream.JetStream
	if enableJetStream {
		js, err = jetstream.New(nc)
		if err != nil {
			nc.Close()
			srv.Shutdown()
			t.Fatalf("Failed to create JetStream context: %v", err)
		}
	}

	cleanup := func() {
		nc.Close()
		srv.Shutdown()
		// srv.WaitForShutdown() // nstest.RunServer's shutdown is synchronous enough for most test cases
	}

	return nc, js, cleanup
}

func TestNATS2SSEHandler_CoreNATS_Embedded(t *testing.T) {
	nc, _, cleanup := setupTestServer(t, false) // JetStream not needed for this test
	defer cleanup()

	handler := &NATS2SSEHandler{
		NATSConn: nc,
		Auth:     testAuthFunc,
		Logger:   log.New(io.Discard, "", 0), // Quieter logger for tests
		// Logger:   log.New(os.Stdout, "test-core-emb: ", log.LstdFlags), // For debugging
	}

	server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))
	defer server.Close()

	sseURL := fmt.Sprintf("%s?token=%s&subject=%s", server.URL, testToken, testSubjectCore)
	req, _ := http.NewRequest("GET", sseURL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected status OK, got %d. Body: %s", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	var receivedMessages []string
	var wg sync.WaitGroup
	wg.Add(1) // For the received message

	// Read SSE events in a goroutine
	go func() {
		defer wg.Done()
		// Wait for connection established message
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Errorf("Error reading initial SSE response: %v", readErr)
			return
		}
		if !strings.HasPrefix(line, ": connection established") {
			t.Errorf("Expected connection established comment, got: %s", line)
			return
		}
		_, _ = reader.ReadString('\n') // Read the second newline

		for i := range 3 { // Expect 1 event line, 1 data line, 1 empty line
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				if errors.Is(readErr, io.EOF) && i > 0 {
					break
				}
				// Check if context was cancelled, which might be why read failed
				if ctx.Err() != nil {
					t.Logf("Context cancelled during SSE read: %v", ctx.Err())
					return
				}
				t.Errorf("Error reading SSE stream (line %d): %v. Received so far: %v", i+1, readErr, receivedMessages)
				return
			}
			trimmedLine := strings.TrimSpace(line)
			if trimmedLine != "" {
				receivedMessages = append(receivedMessages, trimmedLine)
			}
			if strings.HasPrefix(trimmedLine, "data:") {
				break // Found our data
			}
		}
	}()

	// Publish a message to NATS
	time.Sleep(200 * time.Millisecond) // Give SSE connection and reader goroutine a moment
	testPayload := "Hello from CoreNATS Embedded"
	if pubErr := nc.Publish(testSubjectCore, []byte(testPayload)); pubErr != nil {
		t.Fatalf("Failed to publish NATS message: %v", pubErr)
	}
	t.Logf("Published to %s: %s", testSubjectCore, testPayload)

	wg.Wait() // Wait for the reader goroutine to finish or timeout

	// Check received messages after goroutine completes
	if ctx.Err() != nil {
		t.Fatalf("Test context timed out or was cancelled. Received: %v", receivedMessages)
	}

	if len(receivedMessages) < 2 {
		t.Fatalf("Did not receive enough SSE lines. Got: %v", receivedMessages)
	}

	foundEvent := false
	foundData := false
	for _, line := range receivedMessages {
		if line == fmt.Sprintf("event: %s", testSubjectCore) {
			foundEvent = true
		}
		if strings.HasPrefix(line, "data:") {
			encodedData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			decodedData, decErr := base64.StdEncoding.DecodeString(encodedData)
			if decErr != nil {
				t.Fatalf("Failed to decode base64 data '%s': %v", encodedData, decErr)
			}
			if string(decodedData) == testPayload {
				foundData = true
			} else {
				t.Errorf("Expected data '%s', got '%s'", testPayload, string(decodedData))
			}
		}
	}

	if !foundEvent {
		t.Errorf("SSE event line not found. Received: %v", receivedMessages)
	}
	if !foundData {
		t.Errorf("SSE data line with correct payload not found. Received: %v", receivedMessages)
	}
}

func TestNATS2SSEHandler_JetStream_Embedded(t *testing.T) {
	nc, js, cleanup := setupTestServer(t, true) // Enable JetStream
	defer cleanup()

	// Create a stream
	ctxStream, cancelStream := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStream()

	_, err := js.CreateOrUpdateStream(ctxStream, jetstream.StreamConfig{
		Name:     testStreamName,
		Subjects: []string{testSubjectJetStream}, // testSubjectJetStream is "test.js.subject"
	})
	if err != nil {
		t.Fatalf("Failed to create test JetStream stream: %v", err)
	}

	handler := &NATS2SSEHandler{
		NATSConn:      nc,
		Auth:          testAuthFunc,
		IsJetStream:   true,
		JetStreamName: testStreamName,
		Logger:        log.New(io.Discard, "", 0), // Quieter logger
		// Logger:        log.New(os.Stdout, "test-js-emb: ", log.LstdFlags), // For debugging
	}

	server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))
	defer server.Close()

	sseURL := fmt.Sprintf("%s?token=%s&subject=%s", server.URL, testToken, testSubjectJetStream)
	req, _ := http.NewRequest("GET", sseURL, nil)
	ctx, cancelHTTP := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelHTTP()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected status OK, got %d. Body: %s", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	var receivedMessages []string
	var wg sync.WaitGroup
	wg.Add(1) // For received message

	go func() {
		defer wg.Done()
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Errorf("Error reading initial SSE response: %v", readErr)
			return
		}
		if !strings.HasPrefix(line, ": connection established") {
			t.Errorf("Expected connection established comment, got: %s", line)
			return
		}
		_, _ = reader.ReadString('\n')

		for i := range 3 {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				if errors.Is(readErr, io.EOF) && i > 0 {
					break
				}
				if ctx.Err() != nil {
					t.Logf("Context cancelled during SSE read (JS): %v", ctx.Err())
					return
				}
				t.Errorf("Error reading SSE stream (JS line %d): %v. Received so far: %v", i+1, readErr, receivedMessages)
				return
			}
			trimmedLine := strings.TrimSpace(line)
			if trimmedLine != "" {
				receivedMessages = append(receivedMessages, trimmedLine)
			}
			if strings.HasPrefix(trimmedLine, "data:") {
				break
			}
		}
	}()

	// Publish a message to JetStream
	time.Sleep(300 * time.Millisecond) // Give SSE, consumer, and reader goroutine a moment
	testPayload := "Hello from JetStream Embedded"
	pubCtx, pubCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pubCancel()
	_, pubErr := js.Publish(pubCtx, testSubjectJetStream, []byte(testPayload))
	if pubErr != nil {
		t.Fatalf("Failed to publish NATS JetStream message: %v", pubErr)
	}
	t.Logf("Published to JS %s: %s", testSubjectJetStream, testPayload)

	wg.Wait()

	if ctx.Err() != nil {
		t.Fatalf("Test context timed out or was cancelled (JS). Received: %v", receivedMessages)
	}

	if len(receivedMessages) < 2 {
		t.Fatalf("Did not receive enough SSE lines for JetStream. Got: %v", receivedMessages)
	}

	foundEvent := false
	foundData := false
	for _, line := range receivedMessages {
		if line == fmt.Sprintf("event: %s", testSubjectJetStream) {
			foundEvent = true
		}
		if strings.HasPrefix(line, "data:") {
			encodedData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			decodedData, decErr := base64.StdEncoding.DecodeString(encodedData)
			if decErr != nil {
				t.Fatalf("Failed to decode base64 data '%s': %v", encodedData, decErr)
			}
			if string(decodedData) == testPayload {
				foundData = true
			} else {
				t.Errorf("Expected JS data '%s', got '%s'", testPayload, string(decodedData))
			}
		}
	}

	if !foundEvent {
		t.Errorf("SSE JS event line not found. Received: %v", receivedMessages)
	}
	if !foundData {
		t.Errorf("SSE JS data line with correct payload not found. Received: %v", receivedMessages)
	}
}

func TestNATS2SSEHandler_MessageCallback(t *testing.T) {
	nc, _, cleanup := setupTestServer(t, false)
	defer cleanup()

	var callbackHitCount int32
	var deniedCount int32
	var successfulCount int32
	denyMessageContent := "DENY THIS MESSAGE"
	allowMessageContent := "ALLOW THIS MESSAGE"

	callback := func(ctx context.Context, clientID, subject string, msgData []byte) error {
		atomic.AddInt32(&callbackHitCount, 1)
		t.Logf("Callback invoked for client %s, subject %s, data: %s", clientID, subject, string(msgData))
		if strings.Contains(string(msgData), denyMessageContent) {
			atomic.AddInt32(&deniedCount, 1)
			return errors.New("denied by test callback")
		}
		atomic.AddInt32(&successfulCount, 1)
		return nil
	}

	handler := &NATS2SSEHandler{
		NATSConn:        nc,
		Auth:            testAuthFunc,
		Logger:          log.New(io.Discard, "", 0),
		MessageCallback: callback,
	}

	server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))
	defer server.Close()

	sseURL := fmt.Sprintf("%s?token=%s&subject=%s", server.URL, testToken, testSubjectCore)
	req, _ := http.NewRequest("GET", sseURL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected status OK, got %d. Body: %s", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	receivedSSEData := make(chan string, 5) // Channel to send decoded SSE data
	var sseReaderWG sync.WaitGroup
	sseReaderWG.Add(1)

	go func() {
		defer sseReaderWG.Done()
		defer close(receivedSSEData)
		// Consume initial comments
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				t.Logf("SSE reader initial read error (expected if context done): %v", readErr)
				return
			}
			if strings.TrimSpace(line) == "" {
				break // Found the empty line after comments
			}
		}

		// Read actual events
		for {
			select {
			case <-ctx.Done():
				t.Log("SSE reader context done.")
				return
			default:
				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					if errors.Is(readErr, io.EOF) || errors.Is(readErr, context.Canceled) {
						t.Logf("SSE reader stream closed or context cancelled: %v", readErr)
						return
					}
					t.Errorf("Error reading SSE stream: %v", readErr)
					return
				}
				trimmedLine := strings.TrimSpace(line)
				if strings.HasPrefix(trimmedLine, "data:") {
					encodedData := strings.TrimSpace(strings.TrimPrefix(trimmedLine, "data:"))
					decodedData, decErr := base64.StdEncoding.DecodeString(encodedData)
					if decErr != nil {
						t.Errorf("Failed to decode base64 data '%s': %v", encodedData, decErr)
						continue
					}
					receivedSSEData <- string(decodedData)
				}
			}
		}
	}()

	// Give the SSE connection and reader a moment
	time.Sleep(200 * time.Millisecond)

	// Publish message that should be allowed
	if pubErr := nc.Publish(testSubjectCore, []byte(allowMessageContent)); pubErr != nil {
		t.Fatalf("Failed to publish ALLOW message: %v", pubErr)
	}
	t.Logf("Published ALLOW message: %s", allowMessageContent)

	// Publish message that should be denied by the callback
	if pubErr := nc.Publish(testSubjectCore, []byte(denyMessageContent)); pubErr != nil {
		t.Fatalf("Failed to publish DENY message: %v", pubErr)
	}
	t.Logf("Published DENY message: %s", denyMessageContent)

	// Publish another allowed message
	if pubErr := nc.Publish(testSubjectCore, []byte(allowMessageContent+" 2")); pubErr != nil {
		t.Fatalf("Failed to publish ALLOW message 2: %v", pubErr)
	}
	t.Logf("Published ALLOW message 2: %s", allowMessageContent+" 2")

	// Collect received SSE messages with a timeout
	var receivedPayloads []string
	expectedPayloads := []string{allowMessageContent, allowMessageContent + " 2"}
	collectTimeout := time.After(2 * time.Second)

	for len(receivedPayloads) < len(expectedPayloads) {
		select {
		case payload := <-receivedSSEData:
			receivedPayloads = append(receivedPayloads, payload)
		case <-collectTimeout:
			t.Logf("Timeout waiting for all expected SSE messages. Got %d, expected %d.", len(receivedPayloads), len(expectedPayloads))
			break
		case <-ctx.Done():
			t.Log("Test context cancelled during SSE payload collection.")
			break
		}
	}

	// Wait a bit more to ensure no unexpected messages arrive (like the denied one)
	select {
	case unexpected := <-receivedSSEData:
		t.Fatalf("Received unexpected SSE message after collection: %s", unexpected)
	case <-time.After(500 * time.Millisecond):
		// No unexpected messages within the grace period, good.
	case <-ctx.Done():
	}

	cancel()           // Signal http request to finish
	sseReaderWG.Wait() // Wait for the SSE reader goroutine to finish

	// Assertions
	if actual := atomic.LoadInt32(&callbackHitCount); actual != 3 {
		t.Errorf("Expected callback to be hit 3 times, got %d", actual)
	}
	if actual := atomic.LoadInt32(&deniedCount); actual != 1 {
		t.Errorf("Expected 1 message to be denied by callback, got %d", actual)
	}
	if actual := atomic.LoadInt32(&successfulCount); actual != 2 {
		t.Errorf("Expected 2 messages to be successfully processed by callback, got %d", actual)
	}

	if len(receivedPayloads) != 2 {
		t.Fatalf("Expected 2 SSE messages to be sent, got %d. Received: %v", len(receivedPayloads), receivedPayloads)
	}
	if !((receivedPayloads[0] == allowMessageContent && receivedPayloads[1] == allowMessageContent+" 2") ||
		(receivedPayloads[1] == allowMessageContent && receivedPayloads[0] == allowMessageContent+" 2")) {
		t.Errorf("Received payloads mismatch. Expected %v, got %v", expectedPayloads, receivedPayloads)
	}

	// Verify the denied message was NOT received by SSE
	for _, payload := range receivedPayloads {
		if strings.Contains(payload, denyMessageContent) {
			t.Errorf("Denied message '%s' was unexpectedly sent to SSE!", denyMessageContent)
		}
	}
}
