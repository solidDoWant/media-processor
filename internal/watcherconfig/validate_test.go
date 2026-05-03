package watcherconfig

import (
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewValidator_MinLengthRulesEnforced verifies that the runtime length
// constraints registered from `jsonschema:"minLength=N"` tags reject empty
// values for the watcher config string fields.
func TestNewValidator_MinLengthRulesEnforced(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		errFunc require.ErrorAssertionFunc // defaults to require.NoError
	}{
		{
			name: "valid config passes",
			cfg: Config{
				ScanInterval: DefaultScanInterval,
				Watches: []WatchEntry{
					{Name: "movies", WatchedPath: "/watch", MediaType: "movie", Output: WatchEntryOutput{Path: "/out"}},
				},
			},
		},
		{
			name: "empty Name is rejected",
			cfg: Config{
				ScanInterval: DefaultScanInterval,
				Watches: []WatchEntry{
					{Name: "", WatchedPath: "/watch", MediaType: "movie", Output: WatchEntryOutput{Path: "/out"}},
				},
			},
			errFunc: require.Error,
		},
		{
			name: "empty WatchedPath is rejected",
			cfg: Config{
				ScanInterval: DefaultScanInterval,
				Watches: []WatchEntry{
					{Name: "movies", WatchedPath: "", MediaType: "movie", Output: WatchEntryOutput{Path: "/out"}},
				},
			},
			errFunc: require.Error,
		},
		{
			name: "empty Output.Path is rejected",
			cfg: Config{
				ScanInterval: DefaultScanInterval,
				Watches: []WatchEntry{
					{Name: "movies", WatchedPath: "/watch", MediaType: "movie", Output: WatchEntryOutput{Path: ""}},
				},
			},
			errFunc: require.Error,
		},
	}

	v := NewValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			errFunc := tt.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, v.Struct(tt.cfg))
		})
	}
}

// TestRegisterSchemaConstraints_EmptyStringRule verifies that a struct with a
// `jsonschema:"minLength=N"` tag has its rule applied at runtime by the helper.
func TestRegisterSchemaConstraints_EmptyStringRule(t *testing.T) {
	t.Parallel()

	type sample struct {
		Field string `jsonschema:"minLength=3"`
	}

	v := validator.New(validator.WithRequiredStructEnabled())
	registerSchemaConstraints(v, sample{})

	assert.NoError(t, v.Struct(sample{Field: "abc"}))
	assert.Error(t, v.Struct(sample{Field: "ab"}))
	assert.Error(t, v.Struct(sample{Field: ""}))
}

// TestRegisterSchemaConstraints_IgnoresNonStringFields verifies that the helper
// only applies length rules to string fields (other types' tags are ignored).
func TestRegisterSchemaConstraints_IgnoresNonStringFields(t *testing.T) {
	t.Parallel()

	type sample struct {
		Field int `jsonschema:"minLength=5"`
	}

	v := validator.New(validator.WithRequiredStructEnabled())
	registerSchemaConstraints(v, sample{})

	assert.NoError(t, v.Struct(sample{Field: 0}))
}

// TestParseMinLength_PanicsOnMalformed verifies that a non-numeric minLength
// value panics so misconfigured tags surface at validator construction time.
func TestParseMinLength_PanicsOnMalformed(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() {
		parseMinLength("minLength=abc")
	})
}

// TestParseMinLength_NoMinLengthOption verifies that a tag without a minLength
// option returns ok=false rather than producing a rule.
func TestParseMinLength_NoMinLengthOption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tag  string
	}{
		{name: "empty tag", tag: ""},
		{name: "unrelated option", tag: "format=regex"},
		{name: "multiple unrelated options", tag: "format=regex,description=foo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, ok := parseMinLength(tt.tag)
			assert.False(t, ok)
		})
	}
}
