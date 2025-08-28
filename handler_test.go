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

	nstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	testSubjectJetStream = "test.js.subject"
	testStreamName       = "TESTSTREAM"
	testClientID         = "test-client-123"
	testToken            = "testtoken"
)

// Helper function to setup an embedded NATS server with JetStream.
func setupJetStreamServer(t *testing.T) (nc *nats.Conn, js jetstream.JetStream, cleanupFunc func()) {
	t.Helper()

	opts := nstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()

	srv := nstest.RunServer(&opts)

	var err error
	nc, err = nats.Connect(srv.ClientURL(), nats.Timeout(5*time.Second))
	if err != nil {
		srv.Shutdown()
		t.Fatalf("Failed to connect to NATS server at %s: %v", srv.ClientURL(), err)
	}

	js, err = jetstream.New(nc)
	if err != nil {
		nc.Close()
		srv.Shutdown()
		t.Fatalf("Failed to create JetStream context: %v", err)
	}

	cleanup := func() {
		nc.Close()
		srv.Shutdown()
	}

	return nc, js, cleanup
}

func createTestStream(t *testing.T, js jetstream.JetStream, streamName, subject string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	if err != nil {
		t.Fatalf("Failed to create test JetStream stream '%s': %v", streamName, err)
	}
}

// readAllSSEResponses reads from an SSE stream until the context is done or an error occurs.
// It decodes data payloads and returns them.
func readAllSSEResponses(t *testing.T, ctx context.Context, body io.Reader) []string {
	t.Helper()
	reader := bufio.NewReader(body)
	var receivedPayloads []string
	var mu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					t.Logf("Error reading from SSE stream: %v", err)
				}
				return
			}

			if strings.HasPrefix(line, "data:") {
				encodedData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				decodedData, decErr := base64.StdEncoding.DecodeString(encodedData)
				if decErr != nil {
					t.Errorf("Failed to decode base64 data '%s': %v", encodedData, decErr)
					continue
				}
				mu.Lock()
				receivedPayloads = append(receivedPayloads, string(decodedData))
				mu.Unlock()
			}
		}
	}()

	<-ctx.Done() // Wait until the context is cancelled
	wg.Wait()    // Wait for the reader to finish processing

	mu.Lock()
	defer mu.Unlock()
	return receivedPayloads
}

// Dummy SujectFunc for testing
func testSubjectFunc(r *http.Request) (subject string, clientID string, since *time.Time, err error) {
	token := r.URL.Query().Get("token")
	subj := r.URL.Query().Get("subject")
	sinceStr := r.URL.Query().Get("since")

	if token != testToken {
		return "", "", nil, errors.New("invalid test token")
	}
	if subj == "" {
		return "", "", nil, errors.New("subject parameter required for test")
	}
	if sinceStr != "" {
		t, errParse := time.Parse(time.RFC3339, sinceStr)
		if errParse != nil {
			return "", "", nil, fmt.Errorf("invalid since parameter format: %w", errParse)
		}
		since = &t
	}
	return subj, testClientID, since, nil
}

func TestNATS2SSEHandler_JetStream_Embedded(t *testing.T) {
	nc, js, cleanup := setupJetStreamServer(t)
	defer cleanup()
	createTestStream(t, js, testStreamName, testSubjectJetStream)

	handler := &NATS2SSEHandler{
		natsConn:    nc,
		subjectFunc: testSubjectFunc,
		logger:      log.New(io.Discard, "", 0),
	}

	server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))
	defer server.Close()

	sseURL := fmt.Sprintf("%s?token=%s&subject=%s", server.URL, testToken, testSubjectJetStream)
	req, _ := http.NewRequest("GET", sseURL, nil)
	ctx, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
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
	wg.Add(1)

	go func() {
		defer wg.Done()
		// Skip comments
		for {
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
		}
		// Read one message event
		for i := 0; i < 3; i++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
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

	time.Sleep(300 * time.Millisecond) // Give SSE consumer time to start
	testPayload := "Hello from JetStream Embedded"
	pubCtx, pubCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pubCancel()
	_, pubErr := js.Publish(pubCtx, testSubjectJetStream, []byte(testPayload))
	if pubErr != nil {
		t.Fatalf("Failed to publish NATS JetStream message: %v", pubErr)
	}

	wg.Wait()

	var foundData bool
	for _, line := range receivedMessages {
		if strings.HasPrefix(line, "data:") {
			encodedData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			decodedData, _ := base64.StdEncoding.DecodeString(encodedData)
			if string(decodedData) == testPayload {
				foundData = true
				break
			}
		}
	}
	if !foundData {
		t.Errorf("SSE data line with correct payload not found. Received: %v", receivedMessages)
	}
}

func TestNATS2SSEHandler_JetStream_SinceFilter(t *testing.T) {
	nc, js, cleanup := setupJetStreamServer(t)
	defer cleanup()
	createTestStream(t, js, testStreamName, testSubjectJetStream)

	pubCtx, pubCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pubCancel()

	// Publish an old message that should be filtered
	_, err := js.Publish(pubCtx, testSubjectJetStream, []byte("old message"))
	if err != nil {
		t.Fatalf("Failed to publish old message: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	sinceTime := time.Now()
	time.Sleep(100 * time.Millisecond)

	// Publish a new message that should be received
	expectedPayload := "new message"
	_, err = js.Publish(pubCtx, testSubjectJetStream, []byte(expectedPayload))
	if err != nil {
		t.Fatalf("Failed to publish new message: %v", err)
	}

	handler := &NATS2SSEHandler{
		natsConn:      nc,
		subjectFunc:   testSubjectFunc,
		jetStreamName: testStreamName,
		logger:        log.New(io.Discard, "", 0),
	}

	server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))
	defer server.Close()

	sinceParam := sinceTime.UTC().Format(time.RFC3339)
	sseURL := fmt.Sprintf("%s?token=%s&subject=%s&since=%s", server.URL, testToken, testSubjectJetStream, sinceParam)
	req, _ := http.NewRequest("GET", sseURL, nil)
	ctx, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
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

	receivedPayloads := readAllSSEResponses(t, ctx, resp.Body)

	if len(receivedPayloads) != 1 {
		t.Fatalf("Expected to receive 1 message, but got %d. Payloads: %v", len(receivedPayloads), receivedPayloads)
	}

	if receivedPayloads[0] != expectedPayload {
		t.Errorf("Expected payload '%s', got '%s'", expectedPayload, receivedPayloads[0])
	}
}

func TestNATS2SSEHandler_MessageCallback(t *testing.T) {
	nc, js, cleanup := setupJetStreamServer(t)
	defer cleanup()
	createTestStream(t, js, testStreamName, testSubjectJetStream)

	var callbackHitCount int32
	var deniedCount int32
	var modifiedCount int32
	var errorCount int32
	var allowedCount int32

	callback := func(ctx context.Context, clientID, subject string, headers nats.Header, msgData []byte) ([]byte, error) {
		atomic.AddInt32(&callbackHitCount, 1)

		payload := string(msgData)
		if strings.Contains(payload, "DENY") {
			atomic.AddInt32(&deniedCount, 1)
			return nil, nil // Filter this message
		}
		if strings.Contains(payload, "ERROR") {
			atomic.AddInt32(&errorCount, 1)
			return nil, errors.New("callback error") // Signal an error
		}
		if strings.Contains(payload, "MODIFY") {
			atomic.AddInt32(&modifiedCount, 1)
			return []byte("MODIFIED PAYLOAD"), nil // Modify payload
		}

		atomic.AddInt32(&allowedCount, 1)
		return msgData, nil // Allow as-is
	}

	handler := &NATS2SSEHandler{
		natsConn:        nc,
		subjectFunc:     testSubjectFunc,
		jetStreamName:   testStreamName,
		logger:          log.New(io.Discard, "", 0),
		messageCallback: callback,
	}

	server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))
	defer server.Close()

	sseURL := fmt.Sprintf("%s?token=%s&subject=%s", server.URL, testToken, testSubjectJetStream)
	req, _ := http.NewRequest("GET", sseURL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	// Give the SSE connection a moment to establish
	time.Sleep(300 * time.Millisecond)

	pubCtx, pubCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pubCancel()
	js.Publish(pubCtx, testSubjectJetStream, []byte("ALLOW this message"))
	js.Publish(pubCtx, testSubjectJetStream, []byte("MODIFY this message"))
	js.Publish(pubCtx, testSubjectJetStream, []byte("DENY this message"))
	js.Publish(pubCtx, testSubjectJetStream, []byte("Cause an ERROR with this message"))

	// Collect results
	receivedPayloads := readAllSSEResponses(t, ctx, resp.Body)

	// Final Assertions
	if actual := atomic.LoadInt32(&callbackHitCount); actual != 4 {
		t.Errorf("Expected callback to be hit 4 times, got %d", actual)
	}
	if actual := atomic.LoadInt32(&allowedCount); actual != 1 {
		t.Errorf("Expected 1 message to be allowed, got %d", actual)
	}
	if actual := atomic.LoadInt32(&deniedCount); actual != 1 {
		t.Errorf("Expected 1 message to be denied, got %d", actual)
	}
	if actual := atomic.LoadInt32(&modifiedCount); actual != 1 {
		t.Errorf("Expected 1 message to be modified, got %d", actual)
	}
	if actual := atomic.LoadInt32(&errorCount); actual != 1 {
		t.Errorf("Expected 1 message to error, got %d", actual)
	}

	if len(receivedPayloads) != 2 {
		t.Fatalf("Expected 2 SSE messages to be sent, got %d. Received: %v", len(receivedPayloads), receivedPayloads)
	}

	expectedPayloads := map[string]bool{
		"ALLOW this message": true,
		"MODIFIED PAYLOAD":   true,
	}
	for _, p := range receivedPayloads {
		if !expectedPayloads[p] {
			t.Errorf("Received unexpected payload: %s", p)
		}
	}
}
