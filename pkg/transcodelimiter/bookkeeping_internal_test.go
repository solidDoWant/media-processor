package transcodelimiter

// White-box test for permit-level in-flight bookkeeping. Lives in the
// package itself (not transcodelimiter_test) so it can drive the unexported
// markUsed/release helpers directly: external code cannot construct a
// worker.SlotReleaseInfo because its Reason() method returns the SDK's
// internal SlotReleaseReason type, which is not re-exported.

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"
)

// internalFakeSampler is a duplicate of the limiter_test.go fakeSampler,
// kept here so this white-box file is self-contained and the public-API
// test file (in package transcodelimiter_test) does not need to export it.
type internalFakeSampler struct {
	value atomic.Uint64

	mu     sync.Mutex
	failed chan struct{}
}

func newInternalFakeSampler(initial float64) *internalFakeSampler {
	s := &internalFakeSampler{failed: make(chan struct{})}
	s.value.Store(math.Float64bits(initial))

	return s
}

func (s *internalFakeSampler) Value() float64 {
	return math.Float64frombits(s.value.Load())
}

func (s *internalFakeSampler) FailedC() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.failed
}

// TestReleaseOfUnmarkedPermitDoesNotDecrementInFlight is the regression test
// for the bug where Temporal's "reservation cancelled, slot was unused" path
// would call ReleaseSlot for a permit that MarkSlotUsed never saw. The old
// limiter decremented inFlight unconditionally on every release, which
// allowed an unused-permit release to cancel out an unrelated marked permit
// and let admission overshoot StaticCap.
func TestReleaseOfUnmarkedPermitDoesNotDecrementInFlight(t *testing.T) {
	sampler := newInternalFakeSampler(0.1)

	lim, err := New(Config{
		StaticCap:             5,
		GPUThreshold:          0.8,
		PostAdmissionCooldown: time.Nanosecond,
	}, sampler, nil, WithPollInterval(time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	// 1. Reserve permit A and mark it used. inFlight should be 1.
	permitA, err := lim.ReserveSlot(t.Context(), nil)
	require.NoError(t, err)
	lim.markUsed(permitA)
	require.Equal(t, 1, lim.inFlightSnapshot(), "after MarkSlotUsed(A)")

	// 2. Reserve permit B but do NOT mark it used. inFlight stays at 1
	//    (only MarkSlotUsed increments).
	permitB, err := lim.ReserveSlot(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, lim.inFlightSnapshot(), "after ReserveSlot(B) without MarkSlotUsed")

	// 3. Release permit B (the unused-permit path). inFlight must remain
	//    at 1: B was never counted as in-flight, so releasing it must not
	//    cancel out A.
	lim.release(permitB)
	require.Equal(t, 1, lim.inFlightSnapshot(),
		"unused-permit release must leave inFlight unchanged; releasing B cancelled A's mark")

	// 4. Release permit A normally. inFlight returns to 0.
	lim.release(permitA)
	require.Equal(t, 0, lim.inFlightSnapshot(), "after release(A)")
}

// TestDoubleMarkSlotUsedOnSamePermitIsIdempotent guards against double-counting
// if MarkSlotUsed is somehow called twice for the same permit. Temporal does
// not call MarkSlotUsed twice in normal operation, but the bookkeeping must
// not silently inflate inFlight if it ever did.
func TestDoubleMarkSlotUsedOnSamePermitIsIdempotent(t *testing.T) {
	sampler := newInternalFakeSampler(0.1)

	lim, err := New(Config{
		StaticCap:             5,
		GPUThreshold:          0.8,
		PostAdmissionCooldown: time.Nanosecond,
	}, sampler, nil, WithPollInterval(time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	permit := &worker.SlotPermit{}
	lim.markUsed(permit)
	lim.markUsed(permit) // second call — must not double-count.

	require.Equal(t, 1, lim.inFlightSnapshot(), "duplicate MarkSlotUsed for the same permit must not inflate inFlight")
}

// TestReleaseOfMarkedPermitDecrementsInFlight confirms the marked-set path
// is exercised end-to-end (not just the nil-permit fallback used by the
// public-API tests).
func TestReleaseOfMarkedPermitDecrementsInFlight(t *testing.T) {
	sampler := newInternalFakeSampler(0.1)

	lim, err := New(Config{
		StaticCap:             5,
		GPUThreshold:          0.8,
		PostAdmissionCooldown: time.Nanosecond,
	}, sampler, nil, WithPollInterval(time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	permit, err := lim.ReserveSlot(t.Context(), nil)
	require.NoError(t, err)
	lim.markUsed(permit)
	require.Equal(t, 1, lim.inFlightSnapshot())

	lim.release(permit)
	require.Equal(t, 0, lim.inFlightSnapshot())
}
