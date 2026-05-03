// Package envvar centralises the small env-var validation helpers used by the
// watcher and worker entry points. Each helper returns a descriptive error
// rather than calling os.Exit, so callers retain control of process exit
// semantics.
package envvar

import (
	"fmt"
	"os"
	"strconv"
)

// RequireEnv returns the value of the named environment variable. It returns
// an error of the form "NAME is not set" when the variable is unset or empty,
// matching the wording previously emitted by the watcher and worker startup
// checks.
func RequireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is not set", name)
	}

	return v, nil
}

// ParseBool reads a boolean from the named environment variable. When the
// variable is unset or empty, defaultVal is returned. Otherwise the value is
// parsed via strconv.ParseBool, which accepts 1, t, T, TRUE, true, True, 0, f,
// F, FALSE, false, False. Any other value returns an error consistent with
// the other env-var parsers in the codebase.
func ParseBool(name string, defaultVal bool) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return defaultVal, nil
	}

	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (got %q): %w", name, raw, err)
	}

	return v, nil
}
