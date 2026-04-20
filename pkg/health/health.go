package health

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
)

// Server serves /healthz (always 200) and /readyz (503 until SetReady is called).
type Server struct {
	ready atomic.Bool
}

// New starts an HTTP health server on addr. Returns an error if the listener cannot be opened.
func New(addr string) (*Server, error) {
	s := &Server{}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on health addr %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.ready.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})

	srv := &http.Server{Handler: mux}

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "health HTTP server error: %v\n", err)
		}
	}()

	return s, nil
}

// NewFromEnv reads HEALTH_ADDR. Returns (nil, nil) when the variable is unset or empty.
func NewFromEnv() (*Server, error) {
	addr := os.Getenv("HEALTH_ADDR")
	if addr == "" {
		return nil, nil
	}

	return New(addr)
}

// SetReady atomically flips the readiness state to ready. Subsequent /readyz requests return 200.
func (s *Server) SetReady() {
	s.ready.Store(true)
}
