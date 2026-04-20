package health_test

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/health"
)

// freeAddr returns a local TCP address with an available port.
func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	return addr
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url) //nolint:noctx
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })

	return resp
}

func TestNew_StartsServerOnAddr(t *testing.T) {
	addr := freeAddr(t)

	s, err := health.New(t.Context(), addr)
	require.NoError(t, err)
	require.NotNil(t, s)

	resp := get(t, "http://"+addr+"/healthz")
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resp = get(t, "http://"+addr+"/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestHealthz_AlwaysOK(t *testing.T) {
	addr := freeAddr(t)

	s, err := health.New(t.Context(), addr)
	require.NoError(t, err)

	// Before SetReady
	resp := get(t, "http://"+addr+"/healthz")
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// After SetReady
	s.SetReady()

	resp = get(t, "http://"+addr+"/healthz")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestReadyz_NotReadyBefore_ReadyAfterSetReady(t *testing.T) {
	addr := freeAddr(t)

	s, err := health.New(t.Context(), addr)
	require.NoError(t, err)

	resp := get(t, "http://"+addr+"/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	s.SetReady()

	resp = get(t, "http://"+addr+"/readyz")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestNew_InvalidAddr_ReturnsError(t *testing.T) {
	_, err := health.New(t.Context(), "invalid-addr:99999")
	require.Error(t, err)
}
