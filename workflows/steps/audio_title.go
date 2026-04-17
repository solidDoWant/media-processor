package steps

import (
	"fmt"
	"regexp"
	"strings"
)

// stripLanguageName removes every whole-word, case-insensitive occurrence of
// langName from title, then collapses runs of whitespace and trims surrounding
// space. If langName is empty the title is returned unchanged.
func stripLanguageName(title, langName string) string {
	if langName == "" {
		return title
	}

	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(langName) + `\b`)
	stripped := re.ReplaceAllLiteralString(title, "")

	return strings.TrimSpace(strings.Join(strings.Fields(stripped), " "))
}

// channelLabelRe matches a channel configuration label in any supported format,
// along with any associated wrapper characters, ch/CH suffix, and adjacent
// dash/pipe separators. The label format is X.Y where X is the non-LFE channel
// count and Y is 0 or 1.
//
// Supported formats (all case-insensitive for the ch suffix):
//
//	Plain:              5.1
//	Bracket-wrapped:    [5.1]
//	Paren-wrapped:      (5.1)
//	Brace-wrapped:      {5.1}
//	Angle-wrapped:      <5.1>  (stored as &lt;5.1&gt; in some contexts)
//	Single-quoted:      '5.1'
//	Double-quoted:      "5.1"
//	ch/CH suffix:       5.1ch, 5.1CH, 5.1 ch, 5.1 CH
//	Dash-separated:     English - 5.1, 5.1 - English
//	Pipe-separated:     English | 5.1, 5.1 | English
//	Combinations:       [5.1ch], (5.1 CH), etc.
var channelLabelRe = regexp.MustCompile(
	`(?i)` +
		// Optional leading separator (dash or pipe with surrounding spaces)
		`(?:\s*[-|]\s*)?` +
		// Optional opening wrapper character
		`[\[({<"']?` +
		// The X.Y label: Y is restricted to 0 or 1 (LFE count). Matches
		// embedded in larger "X.Y.Z" sequences are filtered in stripChannelConfigLabel.
		`\d+\.[01]` +
		// Optional ch/CH suffix with optional preceding space
		`(?:\s*ch)?` +
		// Optional closing wrapper character
		`[\])}>"']?` +
		// Optional trailing separator (dash or pipe with surrounding spaces)
		`(?:\s*[-|]\s*)?`,
)

// channelConfigLabel returns the channel configuration label (e.g. "5.1", "2.0")
// for a stream with the given channel count and LFE presence.
// X = channelCount - lfeCount; Y = 1 if hasLFE, else 0.
func channelConfigLabel(channelCount int, hasLFE bool) string {
	lfe := 0
	if hasLFE {
		lfe = 1
	}

	nonLFE := channelCount - lfe
	if nonLFE < 0 {
		nonLFE = 0
	}

	return fmt.Sprintf("%d.%d", nonLFE, lfe)
}

// stripChannelConfigLabel removes any channel configuration label (and its
// associated wrapper characters, ch/CH suffix, and adjacent separators) from
// title, then collapses runs of whitespace and trims leading/trailing space.
//
// Matches that are part of a larger "X.Y.Z" sequence (e.g. "7.1.4" for Dolby
// Atmos) are left intact: the X.Y portion is not stripped when it is
// immediately followed by ".<digit>".
func stripChannelConfigLabel(title string) string {
	matches := channelLabelRe.FindAllStringIndex(title, -1)
	if len(matches) == 0 {
		return title
	}

	var buf strings.Builder

	prevEnd := 0

	for _, match := range matches {
		start, end := match[0], match[1]
		// If the matched label is immediately followed by ".<digit>", it is
		// embedded inside a larger X.Y.Z sequence (e.g. "7.1.4"). Leave it alone.
		if end < len(title) && title[end] == '.' && end+1 < len(title) && title[end+1] >= '0' && title[end+1] <= '9' {
			buf.WriteString(title[prevEnd:end])
			prevEnd = end

			continue
		}

		buf.WriteString(title[prevEnd:start])
		buf.WriteByte(' ')

		prevEnd = end
	}

	buf.WriteString(title[prevEnd:])

	return strings.TrimSpace(strings.Join(strings.Fields(buf.String()), " "))
}

// buildAudioStreamTitle returns the audio stream title to write into the output
// file. Any existing channel configuration label and language indicator are
// stripped from sourceTitle, then the parts are reassembled as
// "[content] [langName] [channelLabel]" with empty parts omitted.
//
// When langName is empty the function behaves like the previous channel-config-
// only implementation: strip any existing channel label, append channelLabel.
func buildAudioStreamTitle(sourceTitle, langName, channelLabel string) string {
	stripped := stripChannelConfigLabel(sourceTitle)
	stripped = stripLanguageName(stripped, langName)

	var parts []string
	if stripped != "" {
		parts = append(parts, stripped)
	}

	if langName != "" {
		parts = append(parts, langName)
	}

	if channelLabel != "" {
		parts = append(parts, channelLabel)
	}

	return strings.Join(parts, " ")
}

// buildSubtitleStreamTitle returns the subtitle stream title to write into the
// output file. Any existing language indicator is stripped from sourceTitle,
// then langName is appended. If langName is empty, sourceTitle is returned
// unchanged (including when it is empty).
func buildSubtitleStreamTitle(sourceTitle, langName string) string {
	if langName == "" {
		return sourceTitle
	}

	stripped := stripLanguageName(sourceTitle, langName)
	if stripped == "" {
		return langName
	}

	return stripped + " " + langName
}
