package trackeddownload_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/media-processor/pkg/medialib/internal/trackeddownload"
)

func TestMatchPath(t *testing.T) {
	release := trackeddownload.Record{QueueID: 1, DownloadID: "a", OutputPath: "/mnt/complete/Show.S01E01-GRP", State: "importPending"}
	singleFile := trackeddownload.Record{QueueID: 2, DownloadID: "b", OutputPath: "/mnt/complete/Movie.2020.mkv", State: "importPending"}
	categoryRoot := trackeddownload.Record{QueueID: 3, DownloadID: "c", OutputPath: "/mnt/complete", State: "importPending"}

	tests := []struct {
		name        string
		records     []trackeddownload.Record
		importPath  string
		wantQueueID int64
		wantRecords int
		wantOK      bool
	}{
		{
			name:        "file inside the download's folder",
			records:     []trackeddownload.Record{singleFile, release},
			importPath:  "/mnt/complete/Show.S01E01-GRP/xumq7TyZCqJpYDTTnZkk.mkv",
			wantQueueID: 1, wantRecords: 1, wantOK: true,
		},
		{
			name:        "file nested deeper inside the download's folder",
			records:     []trackeddownload.Record{release},
			importPath:  "/mnt/complete/Show.S01E01-GRP/Sample/file.mkv",
			wantQueueID: 1, wantRecords: 1, wantOK: true,
		},
		{
			name:        "single-file download whose output path is the file",
			records:     []trackeddownload.Record{singleFile},
			importPath:  "/mnt/complete/Movie.2020.mkv",
			wantQueueID: 2, wantRecords: 1, wantOK: true,
		},
		{
			name:       "folder whose name only starts the same does not match",
			records:    []trackeddownload.Record{release},
			importPath: "/mnt/complete/Show.S01E01-GRP2/file.mkv",
		},
		{
			name:        "most specific output path wins over a parent folder",
			records:     []trackeddownload.Record{categoryRoot, release},
			importPath:  "/mnt/complete/Show.S01E01-GRP/file.mkv",
			wantQueueID: 1, wantRecords: 1, wantOK: true,
		},
		{
			name: "two downloads with the same output path are ambiguous",
			records: []trackeddownload.Record{
				release,
				{QueueID: 9, DownloadID: "z", OutputPath: release.OutputPath, State: "importPending"},
			},
			importPath: "/mnt/complete/Show.S01E01-GRP/file.mkv",
		},
		{
			name: "records for several episodes of one download count together",
			records: []trackeddownload.Record{
				{QueueID: 4, DownloadID: "p", OutputPath: "/mnt/complete/Show.S01-GRP", State: "importPending"},
				{QueueID: 5, DownloadID: "p", OutputPath: "/mnt/complete/Show.S01-GRP", State: "importPending"},
			},
			importPath:  "/mnt/complete/Show.S01-GRP/e02.mkv",
			wantQueueID: 4, wantRecords: 2, wantOK: true,
		},
		{
			name:       "already imported downloads are ignored",
			records:    []trackeddownload.Record{{QueueID: 1, DownloadID: "a", OutputPath: release.OutputPath, State: "imported"}},
			importPath: "/mnt/complete/Show.S01E01-GRP/file.mkv",
		},
		{
			name:       "records without a download ID are ignored",
			records:    []trackeddownload.Record{{QueueID: 1, OutputPath: release.OutputPath, State: "importPending"}},
			importPath: "/mnt/complete/Show.S01E01-GRP/file.mkv",
		},
		{
			name:       "records without an output path are ignored",
			records:    []trackeddownload.Record{{QueueID: 1, DownloadID: "a", State: "importPending"}},
			importPath: "/mnt/complete/Show.S01E01-GRP/file.mkv",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := trackeddownload.MatchPath(test.records, test.importPath)
			assert.Equal(t, test.wantOK, ok)

			if test.wantOK {
				assert.Equal(t, test.wantQueueID, got.QueueID)
				assert.Equal(t, test.wantRecords, got.Records)
			}
		})
	}
}

func TestMatchItem(t *testing.T) {
	records := []trackeddownload.Record{
		{QueueID: 1, ItemID: 10, DownloadID: "a", State: "imported"},
		{QueueID: 2, ItemID: 10, DownloadID: "b", State: "importPending"},
		{QueueID: 3, ItemID: 11, DownloadID: "c", State: "importPending"},
	}

	got, ok := trackeddownload.MatchItem(records, 10)
	assert.True(t, ok)
	assert.Equal(t, int64(2), got.QueueID)

	_, ok = trackeddownload.MatchItem(records, 99)
	assert.False(t, ok)

	_, ok = trackeddownload.MatchItem(records, 0)
	assert.False(t, ok)
}
