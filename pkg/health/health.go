package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// Server serves /healthz (always 200) and /readyz (503 until SetReady is called).
type Server struct {
	ready atomic.Bool
}

// New starts an HTTP health server on addr. The server shuts down when ctx is cancelled.
// Returns an error if the listener cannot be opened.
func New(ctx context.Context, addr string) (*Server, error) {
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
		if !s.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Handler: mux}

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "health HTTP server error: %v\n", err)
		}
	}()

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second) //nolint:contextcheck // intentional: process is exiting
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "health HTTP server shutdown error: %v\n", err)
		}
	}()

	return s, nil
}

// SetReady atomically flips the readiness state to ready. Subsequent /readyz requests return 200.
func (s *Server) SetReady() {
	s.ready.Store(true)
}
