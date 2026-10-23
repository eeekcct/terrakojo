package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHTTPServerTimeoutsAndRouting(t *testing.T) {
	called := false
	server := newHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	require.Equal(t, ":8080", server.Addr)
	require.Equal(t, 5*time.Second, server.ReadHeaderTimeout)
	require.Equal(t, 10*time.Second, server.ReadTimeout)
	require.Equal(t, 15*time.Second, server.WriteTimeout)
	require.Equal(t, 60*time.Second, server.IdleTimeout)

	rr := httptest.NewRecorder()
	server.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/webhook", nil))
	require.Equal(t, http.StatusNoContent, rr.Code)
	require.True(t, called)

	called = false
	rr = httptest.NewRecorder()
	server.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/other", nil))
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.False(t, called)
}
