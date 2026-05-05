// Package transcodelimiter implements a Temporal SlotSupplier that gates
// transcode-activity admission by a load-probe-driven controller. The
// supplier admits work while the worker is underutilized; once saturation is
// detected, no further activities are accepted until the worker drains.
//
// Falls back to a configurable static cap when the load probe is unavailable
// or fails. The fallback transition is observable via the
// media_worker_transcode_admission_mode metric.
package transcodelimiter

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/worker"
)

// Sampler is the read-only surface the limiter consumes from a load-probe
// sampler. The concrete *loadprobe.Sampler satisfies it; tests can substitute
// a fake to drive the state machine deterministically.
type Sampler interface {
	// Value returns the smoothed load value in [0, 1].
	Value() float64
	// FailedC returns a channel that is closed when the sampler enters
	// fallback mode (probe init failure or mid-stream sampling failure).
	FailedC() <-chan struct{}
}

// Config holds the operator-tunable parameters of the limiter.
//
// Defaults are applied by New when fields are zero-valued: StaticCap=5,
// GPUThreshold=0.8, PostAdmissionCooldown=3s. Sampler-side fields
// (sample interval, smoothing window) live on loadprobe.SamplerConfig.
//
// PostAdmissionCooldown is treated specially: a zero value means "use the
// default" (so a Config{} struct boots with the documented defaults), and a
// strictly negative value also normalises to the default. Operators wanting
// no cooldown should set a small positive value (e.g. one nanosecond).
type Config struct {
	// StaticCap is the maximum number of in-flight activities, regardless
	// of probe value. Acts as a defensive backstop in probe mode and as the
	// sole gate in fallback mode.
	StaticCap int
	// GPUThreshold is the smoothed load value at or above which the
	// supplier blocks new reservations in probe mode.
	GPUThreshold float64
	// PostAdmissionCooldown is the minimum interval between successive
	// MarkSlotUsed calls; the second is held until the cooldown elapses.
	PostAdmissionCooldown time.Duration
}

const (
	defaultStaticCap             = 5
	defaultGPUThreshold          = 0.8
	defaultPostAdmissionCooldown = 3 * time.Second
	// defaultPollInterval is how often a blocked ReserveSlot re-checks the
	// admission predicate. Short enough that probe-value transitions wake
	// blocked waiters within one tick; long enough that a saturated worker
	// with 5 blocked waiters costs ~50 wakeups/sec.
	defaultPollInterval = 100 * time.Millisecond
)

// Limiter is a Temporal worker.SlotSupplier whose admission decisions are
// driven by a load-probe sampler. Construct with New and attach to a worker
// via worker.NewCompositeTuner(CompositeTunerOptions{ActivitySlotSupplier: limiter}).
//
// Close releases the fallback-watch goroutine; the underlying sampler must be
// closed by the caller separately. The limiter is safe for concurrent use.
type Limiter struct {
	cfg     Config
	sampler Sampler
	logger  *slog.Logger

	now          func() time.Time
	pollInterval time.Duration

	mu        sync.Mutex
	inFlight  int
	lastAdmit time.Time
	fallback  bool
	// marked tracks permits that MarkSlotUsed has been called for. Only a
	// release of a marked permit decrements inFlight; releases of unmarked
	// permits (Temporal's SlotReleaseReasonUnused path) are ignored. Without
	// this set a release of an unused permit would cancel out an unrelated
	// in-flight transcode and let admission overshoot StaticCap.
	marked map[*worker.SlotPermit]struct{}

	closeC    chan struct{}
	closeOnce sync.Once

	metrics *metricSet
}

// Option configures a Limiter at construction time.
type Option func(*Limiter)

// WithLogger sets the slog logger used for fallback-transition log lines. The
// default is slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(l *Limiter) {
		if log != nil {
			l.logger = log
		}
	}
}

// WithNow overrides the time source. Tests inject a controlled clock to make
// cooldown timing deterministic.
func WithNow(now func() time.Time) Option {
	return func(l *Limiter) {
		if now != nil {
			l.now = now
		}
	}
}

// WithPollInterval overrides the bounded re-check interval used while a
// reservation is blocked. Tests use a small value (e.g. 1ms) to keep the
// state machine responsive without sleeping for the production default.
func WithPollInterval(d time.Duration) Option {
	return func(l *Limiter) {
		if d > 0 {
			l.pollInterval = d
		}
	}
}

// New constructs a Limiter that draws admission signals from sampler. The
// supplied registerer is used to publish the four media_worker_transcode_*
// metrics; a nil registerer disables metric emission (useful in tests).
//
// Returns an error if sampler is nil. A sampler that is already in fallback
// state (e.g. constructed via loadprobe.Failed) is accepted: the limiter
// initializes in static-cap mode and the admission_mode gauge reflects this
// from the first scrape.
func New(cfg Config, sampler Sampler, reg prometheus.Registerer, opts ...Option) (*Limiter, error) {
	if sampler == nil {
		return nil, errors.New("transcodelimiter: sampler is required")
	}

	if cfg.StaticCap <= 0 {
		cfg.StaticCap = defaultStaticCap
	}

	if cfg.GPUThreshold <= 0 || cfg.GPUThreshold > 1 {
		cfg.GPUThreshold = defaultGPUThreshold
	}

	if cfg.PostAdmissionCooldown <= 0 {
		cfg.PostAdmissionCooldown = defaultPostAdmissionCooldown
	}

	l := &Limiter{
		cfg:          cfg,
		sampler:      sampler,
		logger:       slog.Default(),
		now:          time.Now,
		pollInterval: defaultPollInterval,
		closeC:       make(chan struct{}),
		marked:       make(map[*worker.SlotPermit]struct{}),
	}

	for _, opt := range opts {
		opt(l)
	}

	// Reflect a sampler that's already in fallback (e.g. loadprobe.Failed)
	// before any metric is registered, so the first scrape sees the right mode.
	select {
	case <-sampler.FailedC():
		l.fallback = true
	default:
	}

	l.metrics = newMetricSet(reg, l)

	go l.watchFallback()

	return l, nil
}

// Close releases the fallback-watch goroutine. Idempotent. The underlying
// sampler is not closed — the caller owns its lifecycle.
func (l *Limiter) Close() {
	l.closeOnce.Do(func() { close(l.closeC) })
}

func (l *Limiter) watchFallback() {
	select {
	case <-l.sampler.FailedC():
		l.mu.Lock()
		alreadyFallback := l.fallback
		l.fallback = true
		l.mu.Unlock()

		if !alreadyFallback {
			l.logger.Warn("transcode limiter entered static-cap mode",
				slog.String("reason", "load-probe sampler failed"),
			)
		}

		l.metrics.setFallback()
	case <-l.closeC:
	}
}

// inFlightSnapshot is read-on-demand by the metrics collector.
func (l *Limiter) inFlightSnapshot() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.inFlight
}

// admissionState returns (admitted, retryAfter) under the limiter mutex.
// retryAfter is the wall-time wait until the cooldown elapses, used to right-
// size the next poll. retryAfter==0 means "waiting on a non-time signal" (a
// release, or a probe-value drop) — the caller still wakes after pollInterval.
func (l *Limiter) admissionState() (admitted bool, retryAfter time.Duration) {
	if l.inFlight >= l.cfg.StaticCap {
		return false, 0
	}

	if l.fallback {
		return true, 0
	}

	if l.sampler.Value() >= l.cfg.GPUThreshold {
		return false, 0
	}

	if !l.lastAdmit.IsZero() {
		if elapsed := l.now().Sub(l.lastAdmit); elapsed < l.cfg.PostAdmissionCooldown {
			return false, l.cfg.PostAdmissionCooldown - elapsed
		}
	}

	return true, 0
}

// ReserveSlot blocks until the limiter can admit an activity or ctx is
// cancelled. The returned permit is opaque — Temporal hands it back via
// MarkSlotUsed and ReleaseSlot for cooldown stamping and in-flight accounting.
func (l *Limiter) ReserveSlot(ctx context.Context, _ worker.SlotReservationInfo) (*worker.SlotPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	blockStart := l.now()

	defer func() {
		l.metrics.addBlockedSeconds(l.now().Sub(blockStart).Seconds())
	}()

	for {
		l.mu.Lock()
		admitted, retryAfter := l.admissionState()
		l.mu.Unlock()

		if admitted {
			return &worker.SlotPermit{}, nil
		}

		wait := l.pollInterval
		if retryAfter > 0 && retryAfter < wait {
			wait = retryAfter
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()

			return nil, ctx.Err()
		case <-l.closeC:
			timer.Stop()

			return nil, errors.New("transcodelimiter: limiter closed")
		case <-timer.C:
		}
	}
}

// TryReserveSlot returns a permit only if the limiter can admit immediately;
// otherwise it returns nil. Used by Temporal for eager activity reservation.
func (l *Limiter) TryReserveSlot(_ worker.SlotReservationInfo) *worker.SlotPermit {
	l.mu.Lock()
	defer l.mu.Unlock()

	admitted, _ := l.admissionState()
	if !admitted {
		return nil
	}

	return &worker.SlotPermit{}
}

// MarkSlotUsed stamps the cooldown clock and increments the in-flight count.
// Temporal calls this once per reserved slot that is actually used; reserved
// slots that are released without use (SlotReleaseReasonUnused) skip this
// call so the cooldown is not consumed by no-op poll cycles.
func (l *Limiter) MarkSlotUsed(info worker.SlotMarkUsedInfo) {
	var permit *worker.SlotPermit
	if info != nil {
		permit = info.Permit()
	}

	l.markUsed(permit)
}

// ReleaseSlot decrements the in-flight count when the permit was previously
// passed through MarkSlotUsed; releases of unmarked permits (Temporal's
// "unused" reservation path) leave the counter alone so they cannot cancel
// out an unrelated in-flight transcode.
func (l *Limiter) ReleaseSlot(info worker.SlotReleaseInfo) {
	var permit *worker.SlotPermit
	if info != nil {
		permit = info.Permit()
	}

	l.release(permit)
}

// markUsed performs the cooldown stamp and in-flight increment. The permit
// argument is recorded in the marked set so the matching ReleaseSlot can be
// distinguished from an unused-permit release. A nil permit (legitimate only
// in tests that exercise the in-flight counter through the public surface)
// still increments the counter — the regression test for permit-level
// bookkeeping uses non-nil permits to verify the marked-set behaviour.
func (l *Limiter) markUsed(permit *worker.SlotPermit) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if permit != nil {
		if _, dup := l.marked[permit]; dup {
			return
		}

		l.marked[permit] = struct{}{}
	}

	l.lastAdmit = l.now()
	l.inFlight++
}

// release decrements the in-flight count when permit is in the marked set
// (or when permit is nil — the test-only path used by callers passing nil
// info). Otherwise the call is a no-op: it represents a release of a permit
// that Temporal never used, and inFlight was never incremented for it.
func (l *Limiter) release(permit *worker.SlotPermit) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if permit == nil {
		// Test-only path: callers driving the limiter without SDK info
		// stubs rely on inc/dec semantics directly.
		if l.inFlight > 0 {
			l.inFlight--
		}

		return
	}

	if _, used := l.marked[permit]; !used {
		return
	}

	delete(l.marked, permit)

	if l.inFlight > 0 {
		l.inFlight--
	}
}

// MaxSlots returns the static cap. Temporal uses this as a defensive
// backstop on the maximum number of slots the supplier will ever issue.
func (l *Limiter) MaxSlots() int {
	return l.cfg.StaticCap
}
