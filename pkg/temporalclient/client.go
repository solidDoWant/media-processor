// Package temporalclient builds a Temporal SDK client configured via the
// envconfig package, with two extensions on top:
//
//   - file:// API keys: when the resolved API key has the form
//     "file:///absolute/path", the path is read on every RPC via
//     [client.NewAPIKeyDynamicCredentials], so external rotators that update
//     the file in place are picked up without a process restart.
//   - Startup health check: [Dial] verifies the gRPC connection to the
//     frontend before returning. The underlying [client.Dial] is lazy and
//     would otherwise return a healthy-looking client even when the server
//     is unreachable.
package temporalclient

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"
)

// healthCheckTimeout is how long Dial waits for the startup CheckHealth probe.
const healthCheckTimeout = 10 * time.Second

// Option configures a Dial call.
type Option func(*config)

type config struct {
	metricsHandler client.MetricsHandler
}

// WithMetricsHandler installs the supplied client.MetricsHandler on the
// Temporal SDK client so SDK-internal counters/gauges/timers flow through
// it. Callers should construct the handler with [NewMetricsHandler] and own
// its lifecycle (the returned closer must be invoked at process shutdown).
// When omitted, SDK metrics are silently dropped.
func WithMetricsHandler(h client.MetricsHandler) Option {
	return func(c *config) { c.metricsHandler = h }
}

// Dial loads Temporal client options via envconfig, expands a file:// API key
// into dynamic credentials when present, and verifies the gRPC connection via
// CheckHealth before returning. Callers must close the returned client.
func Dial(ctx context.Context, opts ...Option) (client.Client, error) {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	clientOpts, err := buildOptions(cfg)
	if err != nil {
		return nil, err
	}

	c, err := client.Dial(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("dial Temporal: %w", err)
	}

	healthCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	if _, err := c.CheckHealth(healthCtx, &client.CheckHealthRequest{}); err != nil {
		c.Close()
		return nil, fmt.Errorf("temporal health check failed: %w", err)
	}

	return c, nil
}

// buildOptions loads client.Options via envconfig, replacing a file:// API
// key with dynamic credentials backed by os.ReadFile, and wiring the SDK's
// MetricsHandler/Logger to the host application's observability stack.
func buildOptions(cfg *config) (client.Options, error) {
	profile, err := envconfig.LoadClientConfigProfile(envconfig.LoadClientConfigProfileOptions{})
	if err != nil {
		return client.Options{}, fmt.Errorf("load temporal profile: %w", err)
	}

	apiKeyFile, err := extractAPIKeyFile(&profile)
	if err != nil {
		return client.Options{}, err
	}

	opts, err := profile.ToClientOptions(envconfig.ToClientOptionsRequest{})
	if err != nil {
		return client.Options{}, fmt.Errorf("build temporal client options: %w", err)
	}

	if apiKeyFile != "" {
		opts.Credentials = client.NewAPIKeyDynamicCredentials(apiKeyFileCallback(apiKeyFile))
	}

	if cfg.metricsHandler != nil {
		opts.MetricsHandler = cfg.metricsHandler
	}

	opts.Logger = newLogger()

	return opts, nil
}
