package loadprobe

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// SamplerConfig configures a Sampler.
type SamplerConfig struct {
	// Interval is the wall-time duration between probe samples. Defaults to
	// 500ms when zero.
	Interval time.Duration

	// SmoothingWindow is the number of samples used to derive the EWMA
	// smoothing factor (alpha = 2 / (SmoothingWindow + 1)). Defaults to 5
	// when zero or negative.
	SmoothingWindow int

	// Logger receives a warning when the sampler enters fallback mode. If
	// nil, slog.Default() is used.
	Logger *slog.Logger
}

const (
	defaultSamplerInterval        = 500 * time.Millisecond
	defaultSamplerSmoothingWindow = 5
)

// Sampler runs a Probe in a background loop and exposes an EWMA-smoothed
// reading plus a fallback signal. The first sample failure (init or
// mid-stream) closes the channel returned by Failed and stops the sampling
// loop; callers can observe the transition either by selecting on Failed or
// by calling FailureReason.
//
// Probe values outside [0, 1] are clamped before being mixed into the EWMA;
// out-of-range readings do not trigger fallback. Sample errors do.
type Sampler struct {
	probe Probe
	cfg   SamplerConfig

	value atomic.Uint64 // EWMA stored as math.Float64bits; 0 until first sample

	failed   chan struct{}
	failOnce sync.Once
	reason   atomic.Pointer[error]

	startMu sync.Mutex
	started bool
	cancel  context.CancelFunc
	done    chan struct{}

	closeMu sync.Mutex
	closed  bool
}

// NewSampler constructs a Sampler around probe. The sampling loop does not
// start until Start is called.
func NewSampler(probe Probe, cfg SamplerConfig) *Sampler {
	cfg = applySamplerDefaults(cfg)

	return &Sampler{
		probe:  probe,
		cfg:    cfg,
		failed: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Failed returns a Sampler that is already in the failed state, with the
// given reason. Callers use this constructor when probe construction itself
// failed, so init failures and mid-stream failures share a single observation
// surface. Close on the returned sampler is a no-op.
func Failed(reason error, log *slog.Logger) *Sampler {
	if log == nil {
		log = slog.Default()
	}

	s := &Sampler{
		cfg:    SamplerConfig{Logger: log},
		failed: make(chan struct{}),
		done:   make(chan struct{}),
	}
	close(s.done)
	s.markFailed(reason)

	return s
}

// Start launches the sampling goroutine. Repeated calls are no-ops. The
// goroutine exits when ctx is cancelled, when Close is called, or when the
// probe returns an error.
func (s *Sampler) Start(ctx context.Context) {
	s.startMu.Lock()
	defer s.startMu.Unlock()

	if s.started || s.probe == nil {
		return
	}

	s.started = true

	loopCtx, cancel := context.WithCancel(ctx)

	s.cancel = cancel
	go s.loop(loopCtx)
}

// Value returns the current EWMA-smoothed reading, in the closed interval
// [0, 1]. Returns 0 before the first successful sample.
func (s *Sampler) Value() float64 {
	return math.Float64frombits(s.value.Load())
}

// FailedC returns a channel that is closed when the sampler enters fallback
// mode. The name avoids colliding with the Failed package-level constructor.
func (s *Sampler) FailedC() <-chan struct{} {
	return s.failed
}

// FailureReason returns the error that triggered fallback, or nil if the
// sampler has not failed.
func (s *Sampler) FailureReason() error {
	if reason := s.reason.Load(); reason != nil {
		return *reason
	}

	return nil
}

// Close stops the sampling loop and closes the underlying probe. Safe to
// call multiple times.
func (s *Sampler) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true

	if s.cancel != nil {
		s.cancel()
		<-s.done
	}

	if s.probe != nil {
		return s.probe.Close()
	}

	return nil
}

func (s *Sampler) loop(ctx context.Context) {
	defer close(s.done)

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	alpha := 2.0 / (float64(s.cfg.SmoothingWindow) + 1.0)
	first := true

	for {
		raw, err := s.probe.Sample(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			s.markFailed(err)

			return
		}

		clamped := clamp01(raw)

		var next float64
		if first {
			next = clamped
			first = false
		} else {
			prev := math.Float64frombits(s.value.Load())
			next = alpha*clamped + (1-alpha)*prev
		}

		s.value.Store(math.Float64bits(next))

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Sampler) markFailed(reason error) {
	s.failOnce.Do(func() {
		s.reason.Store(&reason)
		close(s.failed)

		if s.cfg.Logger != nil {
			s.cfg.Logger.Warn("loadprobe sampler entered fallback", "error", reason)
		}
	})
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}

	if v > 1 {
		return 1
	}

	return v
}

func applySamplerDefaults(cfg SamplerConfig) SamplerConfig {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultSamplerInterval
	}

	if cfg.SmoothingWindow <= 0 {
		cfg.SmoothingWindow = defaultSamplerSmoothingWindow
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return cfg
}
