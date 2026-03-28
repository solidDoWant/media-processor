package shared

import (
	"fmt"
	"regexp"
	"strings"
)

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

	var sb strings.Builder
	prev := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		// If the matched label is immediately followed by ".<digit>", it is
		// embedded inside a larger X.Y.Z sequence (e.g. "7.1.4"). Leave it alone.
		if end < len(title) && title[end] == '.' && end+1 < len(title) && title[end+1] >= '0' && title[end+1] <= '9' {
			sb.WriteString(title[prev:end])
			prev = end
			continue
		}
		sb.WriteString(title[prev:start])
		sb.WriteByte(' ')
		prev = end
	}
	sb.WriteString(title[prev:])

	return strings.TrimSpace(strings.Join(strings.Fields(sb.String()), " "))
}

// buildAudioStreamTitle returns the audio stream title to write into the output
// file. Any existing channel configuration label is stripped from sourceTitle,
// then channelLabel is appended. If sourceTitle is empty (or becomes empty after
// stripping), channelLabel is returned on its own.
func buildAudioStreamTitle(sourceTitle, channelLabel string) string {
	stripped := stripChannelConfigLabel(sourceTitle)
	if stripped == "" {
		return channelLabel
	}
	return stripped + " " + channelLabel
}
