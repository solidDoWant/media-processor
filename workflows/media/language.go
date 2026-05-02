package media

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
// titles. Both "" (absent) and "und" (undetermined) map to "Unknown Language"
// since both mean the same thing to a viewer. All other tags delegate to
// iso639Name.
func audioLangName(tag string) string {
	if tag == "" || tag == "und" {
		return "Unknown Language"
	}

	return iso639Name(tag)
}

// normalizeLangTag treats an absent language tag the same as "und" so that
// disambiguateLang does not count them as distinct languages.
func normalizeLangTag(tag string) string {
	if tag == "" {
		return "und"
	}

	return tag
}

// disambiguateLang reports whether the audio streams span more than one
// distinct language. When true, language names should be included in audio
// track titles so the viewer can tell the tracks apart. Empty tags and "und"
// are treated as the same language (both mean "unknown").
func disambiguateLang(streams []AudioStreamInfo) bool {
	if len(streams) == 0 {
		return false
	}

	first := normalizeLangTag(streams[0].Language)

	for _, s := range streams[1:] {
		if normalizeLangTag(s.Language) != first {
			return true
		}
	}

	return false
}
