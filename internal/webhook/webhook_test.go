package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eeekcct/terrakojo/internal/config"
)

func TestWebhookHandlerRouting(t *testing.T) {
	// Create test config
	testConfig := &config.Config{
		WebhookGithubSecret: "test-secret",
		GitHubToken:         "",
	}

	// Create a test webhook handler
	handler, err := NewHandler(testConfig, nil)
	if err != nil {
		t.Fatalf("Failed to create handler: %v", err)
	}

	tests := []struct {
		name           string
		method         string
		headers        map[string]string
		expectedStatus int
		expectedBody   string
	}{
		{
			name:           "GET method should be rejected",
			method:         "GET",
			headers:        map[string]string{},
			expectedStatus: http.StatusMethodNotAllowed,
			expectedBody:   "",
		},
		{
			name:   "GitHub webhook with ping event",
			method: "POST",
			headers: map[string]string{
				"X-GitHub-Event":      "ping",
				"X-GitHub-Delivery":   "test-delivery-123",
				"X-Hub-Signature-256": "sha256=test-signature",
			},
			expectedStatus: http.StatusInternalServerError, // Will fail signature check
			expectedBody:   "",
		},
		{
			name:   "GitLab webhook (not implemented)",
			method: "POST",
			headers: map[string]string{
				"X-GitLab-Event": "Push Hook",
			},
			expectedStatus: http.StatusInternalServerError,
			expectedBody:   "",
		},
		{
			name:           "Unknown webhook source",
			method:         "POST",
			headers:        map[string]string{},
			expectedStatus: http.StatusInternalServerError,
			expectedBody:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, "/webhook", bytes.NewReader([]byte("{}")))
			if err != nil {
				t.Fatal(err)
			}

			// Set headers
			for key, value := range tt.headers {
				req.Header.Set(key, value)
			}

			// Create response recorder
			rr := httptest.NewRecorder()

			// Call the handler
			handler.ServeHTTP(rr, req)

			// Check status code
			if rr.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, rr.Code)
			}
		})
	}
}

func TestWebhookHandlerMethodValidation(t *testing.T) {
	// Create test config
	testConfig := &config.Config{
		WebhookGithubSecret: "test-secret",
		GitHubToken:         "",
	}

	handler, err := NewHandler(testConfig, nil)
	if err != nil {
		t.Fatalf("Failed to create handler: %v", err)
	}

	// Test non-POST methods
	methods := []string{"GET", "PUT", "DELETE", "PATCH"}
	for _, method := range methods {
		t.Run("Method_"+method, func(t *testing.T) {
			req, err := http.NewRequest(method, "/webhook", nil)
			if err != nil {
				t.Fatal(err)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("Expected status %d for %s method, got %d",
					http.StatusMethodNotAllowed, method, rr.Code)
			}
		})
	}
}

func TestWebhookHandlerPlatformDetection(t *testing.T) {
	// Create test config
	testConfig := &config.Config{
		WebhookGithubSecret: "test-secret",
		GitHubToken:         "",
	}

	handler, err := NewHandler(testConfig, nil)
	if err != nil {
		t.Fatalf("Failed to create handler: %v", err)
	}

	tests := []struct {
		name     string
		header   string
		value    string
		expected int
	}{
		{
			name:     "GitHub webhook detection",
			header:   "X-GitHub-Event",
			value:    "push",
			expected: http.StatusInternalServerError, // Will fail parsing without proper payload/signature
		},
		{
			name:     "GitLab webhook detection",
			header:   "X-GitLab-Event",
			value:    "Push Hook",
			expected: http.StatusInternalServerError,
		},
		{
			name:     "Unknown platform",
			header:   "X-Custom-Event",
			value:    "custom",
			expected: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("POST", "/webhook", bytes.NewReader([]byte("{}")))
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set(tt.header, tt.value)

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.expected {
				t.Errorf("Expected status %d, got %d", tt.expected, rr.Code)
			}
		})
	}
}

func TestGitHubPingEventWithValidSignature(t *testing.T) {
	// Create test config with known secret
	testSecret := "test-secret"
	testConfig := &config.Config{
		WebhookGithubSecret: testSecret,
		GitHubToken:         "",
	}

	handler, err := NewHandler(testConfig, nil)
	if err != nil {
		t.Fatalf("Failed to create handler: %v", err)
	}

	// Create a valid GitHub ping payload
	pingPayload := `{"zen": "Non-blocking is better than blocking.", "hook_id": 123}`

	// Calculate proper HMAC-SHA256 signature for webhook verification
	h := hmac.New(sha256.New, []byte(testSecret))
	h.Write([]byte(pingPayload))
	signature := fmt.Sprintf("sha256=%x", h.Sum(nil))

	req, err := http.NewRequest("POST", "/webhook", bytes.NewReader([]byte(pingPayload)))
	if err != nil {
		t.Fatal(err)
	}

	// Set GitHub ping event headers with valid signature
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", "test-delivery-123")
	req.Header.Set("X-Hub-Signature-256", signature)
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Call the handler
	handler.ServeHTTP(rr, req)

	// With valid signature, we should get 200 OK for ping
	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d for valid ping with signature, got %d. Response: %s",
			http.StatusOK, rr.Code, rr.Body.String())
	}

	if rr.Body.String() != "OK" {
		t.Errorf("Expected 'OK' response body, got %s", rr.Body.String())
	}
}

func TestGitHubPingEvent(t *testing.T) {
	// Create test config with known secret
	testConfig := &config.Config{
		WebhookGithubSecret: "test-secret",
		GitHubToken:         "",
	}

	handler, err := NewHandler(testConfig, nil)
	if err != nil {
		t.Fatalf("Failed to create handler: %v", err)
	}

	// Create a valid GitHub ping payload
	pingPayload := `{"zen": "Non-blocking is better than blocking.", "hook_id": 123}`

	// Create proper HMAC signature for the test secret
	// For testing purposes, we'll create a request with proper structure
	req, err := http.NewRequest("POST", "/webhook", bytes.NewReader([]byte(pingPayload)))
	if err != nil {
		t.Fatal(err)
	}

	// Set GitHub ping event headers
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", "test-delivery-123")
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Call the handler
	handler.ServeHTTP(rr, req)

	// The test will fail signature validation, but we can verify it reaches the right code path
	// We expect StatusInternalServerError due to signature validation failure (security improvement)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("Expected status %d (signature validation should fail), got %d",
			http.StatusInternalServerError, rr.Code)
	}

	t.Log("Ping event test completed - signature validation behaved as expected")
}

func TestNewHandler(t *testing.T) {
	// Test handler creation with valid config
	testConfig := &config.Config{
		WebhookGithubSecret: "test-secret",
		GitHubToken:         "test-token",
	}

	handler, err := NewHandler(testConfig, nil)
	if err != nil {
		t.Fatalf("Failed to create handler with valid config: %v", err)
	}

	if handler == nil {
		t.Fatal("Expected handler, got nil")
	}

	if handler.githubWebhook == nil {
		t.Fatal("Expected GitHub webhook to be initialized")
	}

	if handler.client == nil {
		t.Log("Kubernetes client is nil (expected for tests)")
	}
}
