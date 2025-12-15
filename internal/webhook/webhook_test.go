package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/eeekcct/terrakojo/api/v1alpha1"
	"github.com/eeekcct/terrakojo/internal/config"
	"github.com/eeekcct/terrakojo/internal/github"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

// ------------------------------------------------------------
// Payload-driven tests
// ------------------------------------------------------------

const testWebhookSecret = "test-secret"

func mustLoadPayload(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	b, err := os.ReadFile(path)
	require.NoError(t, err, "load payload %s", name)
	return b
}

func sign(secret string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return fmt.Sprintf("sha256=%x", h.Sum(nil))
}

func newWebhookHandlerWithRepo(t *testing.T, repo *v1alpha1.Repository) *Handler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(repo).
		WithObjects(repo).
		Build()

	cfg := &config.Config{WebhookGithubSecret: testWebhookSecret}
	h, err := NewHandler(cfg, cl)
	require.NoError(t, err)
	return h
}

func baseRepo(defaultBranch string) *v1alpha1.Repository {
	return &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "repo",
			Namespace: "default",
			Labels: map[string]string{
				"terrakojo.io/owner":     "eeekcct",
				"terrakojo.io/repo-name": "terrakojo",
			},
		},
		Spec: v1alpha1.RepositorySpec{
			Owner:         "eeekcct",
			Name:          "terrakojo",
			Type:          "github",
			DefaultBranch: defaultBranch,
			GitHubSecretRef: v1alpha1.GitHubSecretRef{
				Name: "gh",
			},
		},
	}
}

func makeRequest(t *testing.T, event string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", "/webhook", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "test-delivery")
	req.Header.Set("X-Hub-Signature-256", sign(testWebhookSecret, body))
	return req
}

func TestPushDefaultBranchEnqueuesCommit(t *testing.T) {
	body := mustLoadPayload(t, "github-push-default-branch.json")
	repo := baseRepo("main")
	handler := newWebhookHandlerWithRepo(t, repo)

	req := makeRequest(t, string(github.EventTypePush), body)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	// verify status updated
	cl := handler.client
	var updated v1alpha1.Repository
	require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Name: repo.Name, Namespace: repo.Namespace}, &updated))
	require.Len(t, updated.Status.DefaultBranchCommits, 1)
	require.Equal(t, "main", updated.Status.DefaultBranchCommits[0].Ref)
	require.Equal(t, "1111111111111111111111111111111111111111", updated.Status.DefaultBranchCommits[0].SHA)
	// branchList untouched
	require.Len(t, updated.Status.BranchList, 0)
}

func TestPushFeatureBranchUpdatesBranchList(t *testing.T) {
	body := mustLoadPayload(t, "github-push-feature-branch.json")
	repo := baseRepo("main")
	// Pretend we already had an older SHA
	repo.Status.BranchList = []v1alpha1.BranchInfo{{
		Ref: "feature/add-api",
		SHA: "oldoldoldoldoldoldoldoldoldoldoldoldoldoldol",
	}}
	handler := newWebhookHandlerWithRepo(t, repo)

	req := makeRequest(t, string(github.EventTypePush), body)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var updated v1alpha1.Repository
	require.NoError(t, handler.client.Get(context.Background(), client.ObjectKey{Name: repo.Name, Namespace: repo.Namespace}, &updated))
	require.Len(t, updated.Status.BranchList, 1)
	info := updated.Status.BranchList[0]
	require.Equal(t, "feature/add-api", info.Ref)
	require.Equal(t, "2222222222222222222222222222222222222222", info.SHA)
}

func TestPROpenSyncCloseLifecycle(t *testing.T) {
	openBody := mustLoadPayload(t, "github-pull-request-opened-simple.json")
	syncBody := mustLoadPayload(t, "github-pull-request-synchronize-simple.json")
	closeBody := mustLoadPayload(t, "github-pull-request-closed-merged.json")

	repo := baseRepo("main")
	handler := newWebhookHandlerWithRepo(t, repo)

	// opened
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, makeRequest(t, string(github.EventTypePR), openBody))
	require.Equal(t, http.StatusOK, rr.Code)

	var updated v1alpha1.Repository
	require.NoError(t, handler.client.Get(context.Background(), client.ObjectKey{Name: repo.Name, Namespace: repo.Namespace}, &updated))
	require.Len(t, updated.Status.BranchList, 1)
	require.Equal(t, "feature/auth", updated.Status.BranchList[0].Ref)
	require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", updated.Status.BranchList[0].SHA)

	// synchronize: SHA should update
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, makeRequest(t, string(github.EventTypePR), syncBody))
	require.Equal(t, http.StatusOK, rr.Code)

	require.NoError(t, handler.client.Get(context.Background(), client.ObjectKey{Name: repo.Name, Namespace: repo.Namespace}, &updated))
	require.Len(t, updated.Status.BranchList, 1)
	require.Equal(t, "3333333333333333333333333333333333333333", updated.Status.BranchList[0].SHA)

	// closed: entry removed
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, makeRequest(t, string(github.EventTypePR), closeBody))
	require.Equal(t, http.StatusOK, rr.Code)

	require.NoError(t, handler.client.Get(context.Background(), client.ObjectKey{Name: repo.Name, Namespace: repo.Namespace}, &updated))
	require.Len(t, updated.Status.BranchList, 0)
	require.Len(t, updated.Status.DefaultBranchCommits, 1)
	require.Equal(t, "main", updated.Status.DefaultBranchCommits[0].Ref)
	require.Equal(t, "4444444444444444444444444444444444444444", updated.Status.DefaultBranchCommits[0].SHA)
}

func TestPRFromDefaultBranchDoesNotUpdateBranchList(t *testing.T) {
	openBody := mustLoadPayload(t, "github-pull-request-opened-default-branch.json")

	repo := baseRepo("main")
	handler := newWebhookHandlerWithRepo(t, repo)

	// opened - PR from default branch (edge case)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, makeRequest(t, string(github.EventTypePR), openBody))
	require.Equal(t, http.StatusOK, rr.Code)

	var updated v1alpha1.Repository
	require.NoError(t, handler.client.Get(context.Background(), client.ObjectKey{Name: repo.Name, Namespace: repo.Namespace}, &updated))
	// Default branch should NOT be added to BranchList
	require.Len(t, updated.Status.BranchList, 0)
	// Default branch should NOT be added to DefaultBranchCommits for PR open
	require.Len(t, updated.Status.DefaultBranchCommits, 0)
}

