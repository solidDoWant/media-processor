package watcherconfig

import (
	"github.com/go-playground/validator/v10"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// NewValidator returns a validator configured with the mediatype validator for MediaType values.
func NewValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.RegisterValidation("mediatype", validateMediaType); err != nil {
		panic("failed to register validation \"mediatype\": " + err.Error())
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
