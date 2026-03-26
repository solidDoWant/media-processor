package watcherconfig

import (
	"github.com/go-playground/validator/v10"
	"github.com/robfig/cron/v3"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// sixFieldCronPattern is a loose structural regex for a 6-field cron expression
// (seconds-leading). It merely enforces six whitespace-separated fields and does
// not validate field contents such as ranges, numeric values, or names — many
// syntactically invalid expressions will still match. Used only by
// CronExpression.JSONSchema() to embed an approximate pattern in the generated
// schema for editor tooling. Runtime validation uses cronParser (robfig/cron)
// and is the authoritative source of validity.
const sixFieldCronPattern = `^(\S+ ){5}\S+$`

// cronParser parses Hatchet's seconds-leading 6-field cron expressions,
// matching the format Hatchet registers with robfig/cron internally.
var cronParser = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// NewValidator returns a validator configured with custom validators for MediaType
// values (mediatype) and Hatchet 6-field cron expressions (hatchetcron).
func NewValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.RegisterValidation("mediatype", validateMediaType); err != nil {
		panic("failed to register validation \"mediatype\": " + err.Error())
	}
	if err := v.RegisterValidation("hatchetcron", validateHatchetCron); err != nil {
		panic("failed to register validation \"hatchetcron\": " + err.Error())
	}
	return v
}

// validateMediaType checks that the field value is one of the values in validMediaTypes.
func validateMediaType(fl validator.FieldLevel) bool {
	mt := medialib.MediaType(fl.Field().String())
	for _, valid := range validMediaTypes {
		if mt == valid {
			return true
		}
	}
	return false
}

// validateHatchetCron checks that the field value is a valid Hatchet 6-field cron expression
// using robfig/cron — the same library Hatchet uses internally.
func validateHatchetCron(fl validator.FieldLevel) bool {
	_, err := cronParser.Parse(fl.Field().String())
	return err == nil
}
