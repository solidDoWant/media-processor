package steps

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

// audioLangName returns the display language name for use in audio track
// titles. "und" (undetermined) maps to "Unknown Language" rather than the
// ISO name "Undetermined", which is less clear to end users. All other tags
// delegate to iso639Name.
func audioLangName(tag string) string {
	if tag == "und" {
		return "Unknown Language"
	}

	return iso639Name(tag)
}

// disambiguateLang reports whether the audio streams span more than one
// distinct language tag. When true, language names should be included in
// audio track titles so the viewer can tell the tracks apart.
func disambiguateLang(streams []AudioStreamInfo) bool {
	if len(streams) == 0 {
		return false
	}

	first := streams[0].Language

	for _, s := range streams[1:] {
		if s.Language != first {
			return true
		}
	}

	return false
}
