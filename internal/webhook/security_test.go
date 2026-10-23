package webhook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eeekcct/terrakojo/api/v1alpha1"
	"github.com/eeekcct/terrakojo/internal/config"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWebhookEmptySecretAcceptsUnsignedRequest(t *testing.T) {
	repo := baseRepo()
	handler := newWebhookHandlerWithSecret(t, repo, "")
	body := mustLoadPayload(t, "github-push-default-branch.json")
	req := makeRequestWithSecret(t, "push", body, "")
	require.Empty(t, req.Header.Get("X-Hub-Signature-256"))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var updated v1alpha1.Repository
	require.NoError(t, handler.client.Get(context.Background(), client.ObjectKeyFromObject(repo), &updated))
	require.NotEmpty(t, updated.Annotations[syncRequestAnnotation])
}

func TestWebhookRejectionsDoNotRequestSync(t *testing.T) {
	body := mustLoadPayload(t, "github-push-default-branch.json")
	malformed := []byte(`{"ref": "refs/heads/main", "repository":`)
	tests := []struct {
		name      string
		body      []byte
		signature string
		unsigned  bool
	}{
		{name: "missing signature", body: body},
		{name: "invalid signature", body: body, signature: "sha256=invalid"},
		{name: "wrong secret", body: body, signature: sign("wrong-secret", body)},
		{name: "changed raw bytes", body: append(bytes.Clone(body), ' '), signature: sign(testWebhookSecret, body)},
		{name: "signed malformed JSON", body: malformed, signature: sign(testWebhookSecret, malformed)},
		{name: "missing signature malformed JSON", body: malformed},
		{name: "unsigned mode malformed JSON", body: malformed, unsigned: true},
		{name: "invalid signature malformed JSON", body: malformed, signature: "sha256=invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := baseRepo()
			secret := testWebhookSecret
			if tt.unsigned {
				secret = ""
			}
			handler := newWebhookHandlerWithSecret(t, repo, secret)
			req := makeRequest(t, "push", tt.body)
			req.Header.Set("X-Hub-Signature-256", tt.signature)
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			require.Equal(t, http.StatusInternalServerError, rr.Code)
			requireNoSyncRequested(t, handler, repo)
		})
	}
}

func TestWebhookBodyLimit(t *testing.T) {
	for mode, secret := range map[string]string{"signed": testWebhookSecret, "unsigned": ""} {
		t.Run(mode, func(t *testing.T) {
			payload := mustLoadPayload(t, "github-push-default-branch.json")
			for _, size := range []int64{maxWebhookBodyBytes - 1, maxWebhookBodyBytes, maxWebhookBodyBytes + 1} {
				// JSON whitespace is significant to the HMAC, but not to JSON decoding.
				body := append(bytes.Clone(payload), bytes.Repeat([]byte(" "), int(size)-len(payload))...)
				for _, knownLength := range []bool{true, false} {
					name := "unknown-length"
					if knownLength {
						name = "known-length"
					}
					t.Run(name+"/"+bodyLimitName(size), func(t *testing.T) {
						repo := baseRepo()
						handler := newWebhookHandlerWithSecret(t, repo, secret)
						req := makeRequestWithSecret(t, "push", body, secret)
						tracked := &trackedBody{Reader: bytes.NewReader(body)}
						req.Body = tracked
						if !knownLength {
							req.ContentLength = -1
							req.TransferEncoding = []string{"chunked"}
						}
						rr := httptest.NewRecorder()

						handler.ServeHTTP(rr, req)

						if size > maxWebhookBodyBytes {
							require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
							require.Equal(t, "Webhook payload too large\n", rr.Body.String())
							requireNoSyncRequested(t, handler, repo)
							if knownLength {
								require.Zero(t, tracked.bytesRead, "reject declared oversized bodies without reading")
							}
						} else {
							require.Equal(t, http.StatusOK, rr.Code)
							var updated v1alpha1.Repository
							require.NoError(t, handler.client.Get(context.Background(), client.ObjectKeyFromObject(repo), &updated))
							require.NotEmpty(t, updated.Annotations[syncRequestAnnotation])
						}
						require.LessOrEqual(t, tracked.bytesRead, maxWebhookBodyBytes+1)
						if !knownLength || size <= maxWebhookBodyBytes {
							require.True(t, tracked.closed)
						}
					})
				}
			}
		})
	}
}

func TestWebhookOversizeDrainIsBounded(t *testing.T) {
	for mode, secret := range map[string]string{"signed": testWebhookSecret, "unsigned": ""} {
		t.Run(mode, func(t *testing.T) {
			for _, event := range []string{"push", "unsupported"} {
				t.Run(event, func(t *testing.T) {
					repo := baseRepo()
					handler := newWebhookHandlerWithSecret(t, repo, secret)
					req := makeRequestWithSecret(t, event, nil, secret)
					tracked := &trackedBody{Reader: io.LimitReader(zeroReader{}, maxWebhookBodyBytes*2)}
					req.Body = tracked
					// A smaller declared length must not bypass the actual-read limit.
					req.ContentLength = 1
					rr := httptest.NewRecorder()

					handler.ServeHTTP(rr, req)

					require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
					require.Equal(t, maxWebhookBodyBytes+1, tracked.bytesRead)
					require.True(t, tracked.closed)
					requireNoSyncRequested(t, handler, repo)
				})
			}
		})
	}
}

func TestWebhookBodyReadErrorDoesNotRequestSync(t *testing.T) {
	for mode, secret := range map[string]string{"signed": testWebhookSecret, "unsigned": ""} {
		t.Run(mode, func(t *testing.T) {
			payload := mustLoadPayload(t, "github-push-default-branch.json")
			for _, body := range []io.Reader{
				failingReader{},
				io.MultiReader(bytes.NewReader(payload), failingReader{}),
			} {
				repo := baseRepo()
				handler := newWebhookHandlerWithSecret(t, repo, secret)
				req := makeRequestWithSecret(t, "push", payload, secret)
				tracked := &trackedBody{Reader: body}
				req.Body = tracked
				req.ContentLength = -1
				rr := httptest.NewRecorder()

				handler.ServeHTTP(rr, req)

				require.Equal(t, http.StatusBadRequest, rr.Code)
				require.Equal(t, "Failed to read webhook payload\n", rr.Body.String())
				require.True(t, tracked.closed)
				requireNoSyncRequested(t, handler, repo)
			}
		})
	}
}

func newWebhookHandlerWithSecret(t *testing.T, repo *v1alpha1.Repository, secret string) *Handler {
	t.Helper()
	fixture := newWebhookHandlerWithRepo(t, repo)
	handler, err := NewHandler(&config.Config{WebhookGithubSecret: secret}, fixture.client)
	require.NoError(t, err)
	return handler
}

func makeRequestWithSecret(t *testing.T, event string, body []byte, secret string) *http.Request {
	t.Helper()
	req := makeRequest(t, event, body)
	if secret == "" {
		req.Header.Del("X-Hub-Signature-256")
	} else {
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	}
	return req
}

func requireNoSyncRequested(t *testing.T, handler *Handler, repo *v1alpha1.Repository) {
	t.Helper()
	var updated v1alpha1.Repository
	require.NoError(t, handler.client.Get(context.Background(), client.ObjectKeyFromObject(repo), &updated))
	require.Equal(t, repo.Annotations, updated.Annotations)
}

func bodyLimitName(size int64) string {
	switch size {
	case maxWebhookBodyBytes - 1:
		return "below-limit"
	case maxWebhookBodyBytes:
		return "at-limit"
	default:
		return "over-limit"
	}
}

type trackedBody struct {
	io.Reader
	bytesRead int64
	closed    bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.bytesRead += int64(n)
	return n, err
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("test body read failure")
}
