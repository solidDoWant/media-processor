package watcherconfig

import (
	"regexp"

	"github.com/go-playground/validator/v10"
)

// sixFieldCron matches a cron expression with exactly 6 space-separated fields,
// as required by Hatchet's seconds-leading format (e.g. "*/5 * * * * *").
var sixFieldCron = regexp.MustCompile(`^(\S+ ){5}\S+$`)

// NewValidator returns a validator configured with custom validators for WorkflowName
// values (workflowname) and Hatchet 6-field cron expressions (hatchetcron).
func NewValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	_ = v.RegisterValidation("workflowname", validateWorkflowName)
	_ = v.RegisterValidation("hatchetcron", validateHatchetCron)
	return v
}

// validateWorkflowName checks that the field value is one of the values in validWorkflowNames.
func validateWorkflowName(fl validator.FieldLevel) bool {
	wn := WorkflowName(fl.Field().String())
	for _, valid := range validWorkflowNames {
		if wn == valid {
			return true
		}
	}
	return false
}

// validateHatchetCron checks that the field value is a valid Hatchet 6-field cron expression.
func validateHatchetCron(fl validator.FieldLevel) bool {
	return sixFieldCron.MatchString(fl.Field().String())
}
