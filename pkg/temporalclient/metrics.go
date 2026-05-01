package temporalclient

import (
	"io"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/uber-go/tally/v4"
	tallyprom "github.com/uber-go/tally/v4/prometheus"
	"go.temporal.io/sdk/client"
	contribtally "go.temporal.io/sdk/contrib/tally"
)

// reportInterval is how often the tally root scope flushes accumulated
// counter/gauge/timer values to its underlying Prometheus reporter. The
// reporter is cached, so flushes are cheap (in-process map writes onto
// existing CounterVec/HistogramVec instances) — the interval just bounds
// how stale a /metrics scrape can see SDK metrics.
const reportInterval = time.Second

// noopCloser fits io.Closer when there is nothing to close, so callers can
// always defer the closer regardless of whether SDK metrics are enabled.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// NewMetricsHandler returns a Temporal SDK MetricsHandler backed by
// go.temporal.io/sdk/contrib/tally and the tally→Prometheus reporter from
// github.com/uber-go/tally. SDK-emitted instruments register on the supplied
// registerer so they appear on the same /metrics endpoint as application
// metrics.
//
// When reg is nil, SDK metrics are dropped via client.MetricsNopHandler. The
// returned closer must be Closed before exit so the tally scope's reporting
// goroutine flushes any pending values and stops cleanly.
func NewMetricsHandler(reg prometheus.Registerer) (client.MetricsHandler, io.Closer) {
	if reg == nil {
		return client.MetricsNopHandler, noopCloser{}
	}

	reporter := tallyprom.NewReporter(tallyprom.Options{
		Registerer: reg,
		// HistogramTimerType produces buckets that are scrapeable into a
		// Prometheus histogram; the tally default is summaries with
		// pre-computed quantiles, which lose information across pods.
		DefaultTimerType: tallyprom.HistogramTimerType,
	})

	scope, closer := tally.NewRootScope(tally.ScopeOptions{
		CachedReporter:  reporter,
		Separator:       tallyprom.DefaultSeparator,
		SanitizeOptions: &contribtally.PrometheusSanitizeOptions,
	}, reportInterval)

	scope = contribtally.NewPrometheusNamingScope(scope)

	return contribtally.NewMetricsHandler(scope), closer
}
