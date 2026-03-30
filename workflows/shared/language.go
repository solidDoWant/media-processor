package shared

import iso639 "github.com/barbashov/iso639-3"

// iso639Name returns the human-readable English name for an ISO 639-2/3 tag.
// If the tag is empty, it returns "". If the tag has no known mapping, the raw
// tag is returned as a fallback so titles remain self-describing.
func iso639Name(tag string) string {
	if tag == "" {
		return ""
	}

	lang := iso639.FromAnyCode(tag)
	if lang == nil {
		return tag
	}

	return lang.Name
}
