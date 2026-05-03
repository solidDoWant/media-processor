package watcherconfig

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-playground/validator/v10"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// NewValidator returns a validator configured with the mediatype validator and with
// runtime length-constraint rules derived from `jsonschema:"minLength=N"` tags on
// the watcher config struct types.
func NewValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.RegisterValidation("mediatype", validateMediaType); err != nil {
		panic("failed to register validation \"mediatype\": " + err.Error())
	}

	registerSchemaConstraints(v, Config{}, WatchEntry{}, WatchEntryOutput{})

	return v
}

// validateMediaType checks that the field value is one of medialib.AllMediaTypes.
func validateMediaType(fl validator.FieldLevel) bool {
	mt := medialib.MediaType(fl.Field().String())
	for _, valid := range medialib.AllMediaTypes() {
		if mt == valid {
			return true
		}
	}

	return false
}

// registerSchemaConstraints scans each struct type for `jsonschema:"minLength=N"` tags
// on string fields and installs a struct-level validator on v that enforces those
// minimum-length rules at runtime. Malformed tags panic at registration time so
// configuration errors fail fast rather than silently allowing invalid values.
func registerSchemaConstraints(v *validator.Validate, types ...any) {
	for _, t := range types {
		rules := extractMinLengthRules(reflect.TypeOf(t))
		if len(rules) == 0 {
			continue
		}

		v.RegisterStructValidation(makeMinLengthStructValidator(rules), t)
	}
}

// minLengthRule captures a single `minLength=N` constraint extracted from a
// jsonschema struct tag.
type minLengthRule struct {
	fieldIndex int
	fieldName  string
	min        int
}

// extractMinLengthRules walks the fields of a struct type and returns one rule
// per string field tagged `jsonschema:"...minLength=N..."`. It panics on a
// malformed minLength value so misconfigured tags surface at startup.
func extractMinLengthRules(t reflect.Type) []minLengthRule {
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("registerSchemaConstraints: %s is not a struct", t))
	}

	var rules []minLengthRule

	for i := range t.NumField() {
		field := t.Field(i)
		if field.Type.Kind() != reflect.String {
			continue
		}

		minLen, ok := parseMinLength(field.Tag.Get("jsonschema"))
		if !ok {
			continue
		}

		rules = append(rules, minLengthRule{
			fieldIndex: i,
			fieldName:  field.Name,
			min:        minLen,
		})
	}

	return rules
}

// parseMinLength extracts the minLength=N value from a comma-separated jsonschema
// tag. It returns (0, false) when no minLength option is present and panics when
// the option is present but malformed.
func parseMinLength(tag string) (int, bool) {
	if tag == "" {
		return 0, false
	}

	const prefix = "minLength="

	for _, part := range strings.Split(tag, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, prefix) {
			continue
		}

		raw := strings.TrimPrefix(part, prefix)

		n, err := strconv.Atoi(raw)
		if err != nil {
			panic(fmt.Sprintf("registerSchemaConstraints: invalid jsonschema minLength %q: %v", raw, err))
		}

		if n < 0 {
			panic(fmt.Sprintf("registerSchemaConstraints: invalid jsonschema minLength %q: must be >= 0", raw))
		}

		return n, true
	}

	return 0, false
}

// makeMinLengthStructValidator returns a struct-level validator that reports an
// error for each rule whose corresponding string field has fewer characters
// (Unicode code points) than the configured minimum. JSON Schema's minLength
// counts characters per RFC 8259, not bytes, so multibyte strings must be
// measured by rune count to keep runtime validation aligned with the schema.
// The reported tag is "min" so downstream error messages match what
// go-playground/validator would emit for `validate:"min=N"`.
func makeMinLengthStructValidator(rules []minLengthRule) validator.StructLevelFunc {
	return func(sl validator.StructLevel) {
		current := sl.Current()
		for _, rule := range rules {
			value := current.Field(rule.fieldIndex)
			if utf8.RuneCountInString(value.String()) < rule.min {
				sl.ReportError(value.Interface(), rule.fieldName, rule.fieldName, "min", strconv.Itoa(rule.min))
			}
		}
	}
}
