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

		for i := 0; i < 3; i++ { // Expect 1 event line, 1 data line, 1 empty line
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

		for i := 0; i < 3; i++ {
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
