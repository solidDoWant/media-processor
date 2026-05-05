package transcodelimiter

import "github.com/prometheus/client_golang/prometheus"

const (
	metricSlotsInFlight       = "media_worker_transcode_slots_in_flight"
	metricSlotsBlockedSeconds = "media_worker_transcode_slots_blocked_seconds_total"
	metricLoadUtilization     = "media_worker_transcode_load_utilization"
	metricAdmissionMode       = "media_worker_transcode_admission_mode"

	admissionModeLabel  = "mode"
	admissionModeProbe  = "probe"
	admissionModeStatic = "static"
)

// metricSet bundles the four Prometheus collectors registered by the limiter.
// Construction is conditional on transcode being enabled — workers without
// the transcode activity skip newMetricSet entirely so no series are emitted.
type metricSet struct {
	blockedSeconds prometheus.Counter
	admissionMode  *prometheus.GaugeVec
}

// newMetricSet registers the four media_worker_transcode_* metrics against
// reg. A nil registerer disables emission entirely (used by tests that don't
// care about metrics).
func newMetricSet(reg prometheus.Registerer, l *Limiter) *metricSet {
	m := &metricSet{
		blockedSeconds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: metricSlotsBlockedSeconds,
			Help: "Cumulative seconds reservations spent blocked on the transcode admission controller.",
		}),
		admissionMode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: metricAdmissionMode,
			Help: "Active admission mode of the transcode supplier. 1 for the active mode, 0 for the other. mode=\"probe\" when the load probe is driving admission, mode=\"static\" when fallen back to static-cap-only.",
		}, []string{admissionModeLabel}),
	}

	// Initialize both label values so the series exist from the first scrape.
	m.admissionMode.WithLabelValues(admissionModeProbe).Set(1)
	m.admissionMode.WithLabelValues(admissionModeStatic).Set(0)

	if l.fallback {
		m.admissionMode.WithLabelValues(admissionModeProbe).Set(0)
		m.admissionMode.WithLabelValues(admissionModeStatic).Set(1)
	}

	if reg == nil {
		return m
	}

	inFlightGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: metricSlotsInFlight,
		Help: "Number of transcodes currently holding a slot.",
	}, func() float64 { return float64(l.inFlightSnapshot()) })

	loadGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: metricLoadUtilization,
		Help: "Smoothed load probe value, in [0, 1]. Reflects the most recent EWMA reading from the configured probe.",
	}, l.sampler.Value)

	reg.MustRegister(m.blockedSeconds, m.admissionMode, inFlightGauge, loadGauge)

	return m
}

func (m *metricSet) addBlockedSeconds(seconds float64) {
	if seconds > 0 {
		m.blockedSeconds.Add(seconds)
	}
}

func (m *metricSet) setFallback() {
	m.admissionMode.WithLabelValues(admissionModeProbe).Set(0)
	m.admissionMode.WithLabelValues(admissionModeStatic).Set(1)
}
