package watcherconfig

import (
	"regexp"

	"github.com/go-playground/validator/v10"
)

// sixFieldCronPattern is the canonical regex for a Hatchet 6-field cron expression.
// It is used by both CronExpression.JSONSchema() (schema generation) and
// validateHatchetCron (runtime validation) to keep the regex in one place.
const sixFieldCronPattern = `^(\S+ ){5}\S+$`

// sixFieldCron is the compiled form of sixFieldCronPattern.
var sixFieldCron = regexp.MustCompile(sixFieldCronPattern)

// NewValidator returns a validator configured with custom validators for WorkflowName
// values (workflowname) and Hatchet 6-field cron expressions (hatchetcron).
func NewValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.RegisterValidation("workflowname", validateWorkflowName); err != nil {
		panic("failed to register validation \"workflowname\": " + err.Error())
	}
	if err := v.RegisterValidation("hatchetcron", validateHatchetCron); err != nil {
		panic("failed to register validation \"hatchetcron\": " + err.Error())
	}
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
