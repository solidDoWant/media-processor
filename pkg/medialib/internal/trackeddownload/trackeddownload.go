// Package trackeddownload finds the Sonarr/Radarr queue entry (the "tracked
// download") that a transcoded file belongs to, and holds the shared rules for
// deciding when a file can't be identified.
//
// The arr's queue is the most reliable source for which movie or episode a
// file is. Every queue entry records the folder the download client put the
// release in (outputPath, already in the arr's own path view). Our output
// mirrors that folder, so the file being imported lives at or under the
// entry's outputPath. Matching on that path works for obfuscated file names
// (e.g. "xumq7TyZCqJpYDTTnZkk.mkv") and for release names the arr's parser
// gets wrong, where parsing the name alone fails or picks the wrong title.
package trackeddownload

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrUnparseable is wrapped (together with medialib.ErrNotFound) when neither
// the file name nor its folder name could be parsed as a release at all. It
// means "we don't know what this is", which is different from "the arr
// recognized the release but has no matching movie or series". Only the
// second case means the item isn't in the library. Callers must not treat
// the first as a reason to skip an import and delete the output.
var ErrUnparseable = errors.New("release name could not be parsed")

// Record is the part of an arr queue record that matching needs.
type Record struct {
	QueueID int64
	// ItemID is the episode ID (Sonarr) or movie ID (Radarr) the record is for.
	ItemID     int64
	DownloadID string
	OutputPath string
	State      string
}

// Download is a matched tracked download.
type Download struct {
	QueueID    int64
	DownloadID string
	// Records is how many queue records share DownloadID. Sonarr has one
	// record per episode, so a season pack has several. Removing a queue
	// item removes the whole download, so only do that when Records is 1.
	Records int
}

// usable reports whether the record can be matched against: it has a
// download ID and the arr hasn't already finished importing it.
func (r Record) usable() bool {
	return r.DownloadID != "" && !strings.EqualFold(r.State, "imported")
}

// MatchPath returns the tracked download whose output path is importPath or
// a parent folder of it. When several records match, the one with the
// longest (most specific) output path wins. If two different downloads tie
// for the longest path, the match is ambiguous and nothing is returned.
func MatchPath(records []Record, importPath string) (Download, bool) {
	importPath = filepath.Clean(importPath)

	var (
		best      Record
		bestLen   = -1
		ambiguous bool
	)

	for _, record := range records {
		if !record.usable() || strings.TrimSpace(record.OutputPath) == "" {
			continue
		}

		outputPath := filepath.Clean(record.OutputPath)
		if outputPath == string(filepath.Separator) || outputPath == "." {
			continue
		}

		if importPath != outputPath && !strings.HasPrefix(importPath, outputPath+string(filepath.Separator)) {
			continue
		}

		switch {
		case len(outputPath) > bestLen:
			best, bestLen, ambiguous = record, len(outputPath), false
		case len(outputPath) == bestLen && record.DownloadID != best.DownloadID:
			ambiguous = true
		}
	}

	if bestLen < 0 || ambiguous {
		return Download{}, false
	}

	return toDownload(records, best), true
}

// MatchItem returns the first usable record for the given episode or movie
// ID. It is the fallback for when no record matches by path, such as a file
// dropped into the watch folder by hand.
func MatchItem(records []Record, itemID int64) (Download, bool) {
	if itemID == 0 {
		return Download{}, false
	}

	for _, record := range records {
		if record.usable() && record.ItemID == itemID {
			return toDownload(records, record), true
		}
	}

	return Download{}, false
}

func toDownload(records []Record, record Record) Download {
	count := 0

	for _, other := range records {
		if other.DownloadID == record.DownloadID {
			count++
		}
	}

	return Download{QueueID: record.QueueID, DownloadID: record.DownloadID, Records: count}
}
