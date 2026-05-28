// Package types defines the shared workflow name and input payload for the media
// processing workflow. It is intentionally free of CGO and FFmpeg dependencies so
// that cmd/watcher can import it without pulling in libav shared libraries.
package types

import "github.com/solidDoWant/media-processor/pkg/medialib"

const MediaWorkflowName = "Media"

// DefaultTaskQueuePrefix is the prefix applied to the workflow task queue and
// every activity task queue. The watcher dispatches workflows to the
// prefix-only queue and the worker derives activity queues from it. Lives
// here (the CGo-free types package) so the watcher can import it without
// pulling in libav.
const DefaultTaskQueuePrefix = "media-processor"

// MediaInput is the workflow's trigger payload.
type MediaInput struct {
	FilePath               string             `json:"file_path"`
	MediaType              medialib.MediaType `json:"media_type"`
	MappingName            string             `json:"mapping_name"`
	PreserveSource         bool               `json:"preserve_source,omitempty"`
	WatchRoot              string             `json:"watch_root,omitempty"`
	RetainEmptyDirectories bool               `json:"retain_empty_directories,omitempty"`
	SkipCropDetection      bool               `json:"skip_crop_detection,omitempty"`
	OutputPath             string             `json:"output_path"`
	OutputRemotePath       string             `json:"output_remote_path,omitempty"`
}
