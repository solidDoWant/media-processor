// Package workflows provides Hatchet workflow definitions.
package workflows

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
)

const movieWorkflowName = "MovieWorkflow"

// MovieWorkflowConfig holds the configuration for the movie processing workflow.
type MovieWorkflowConfig struct {
	// OutputDir is the local directory where transcoded files are written.
	OutputDir string
	// WebhookURL is the endpoint to notify on workflow failure.
	WebhookURL string
}

// MovieInput is the workflow's trigger payload.
type MovieInput struct {
	FilePath string `json:"file_path"`
}

// probeOutput is the output of the probe step.
type probeOutput struct {
	// IsValidMedia is false when the file is not a recognisable media file with a
	// video stream. All downstream steps are skipped when this is false.
	IsValidMedia bool `json:"is_valid_media"`
	// CopyVideo is true when the video stream should be copied without re-encoding
	// (i.e. the source is already H.264 or H.265 in an MKV container).
	CopyVideo bool `json:"copy_video"`
}

// lookupOutput is the output of the lookup step.
type lookupOutput struct {
	MovieID int64 `json:"movie_id"`
}

// transcodeOutput is the output of the transcode step.
type transcodeOutput struct {
	TempPath string `json:"temp_path"`
}

// selectVideoCodec returns CodecCopy when the media is already H.264 or H.265 in an
// MKV container, and CodecH265 otherwise.
func selectVideoCodec(info *ffprobe.MediaInfo) ffmpeg.Codec {
	if strings.Contains(info.Format, "matroska") {
		for _, s := range info.Streams {
			if s.CodecType == ffprobe.CodecTypeVideo {
				if s.CodecName == "h264" || s.CodecName == "hevc" {
					return ffmpeg.CodecCopy
				}
			}
		}
	}
	return ffmpeg.CodecH265
}

// NewMovieWorkflow returns a Hatchet workflow that transcodes a movie file to the
// standard format, moves it to the output directory, and notifies Radarr.
//
// Steps (in order): probe → lookup → transcode → move → notify → cleanup.
// If the source file is not a valid media file with a video stream the file is
// deleted and all other steps are skipped without triggering the failure webhook.
func NewMovieWorkflow(
	client *hatchet.Client,
	cfg MovieWorkflowConfig,
	radarrClient medialib.MovieLibrary,
	webhookClient *webhook.Client,
) *hatchet.Workflow {
	var maxRuns int32 = 1
	dropNewest := types.DropNewest

	wf := client.NewWorkflow(movieWorkflowName,
		hatchet.WithWorkflowConcurrency(types.Concurrency{
			Expression:    "input.file_path",
			MaxRuns:       &maxRuns,
			LimitStrategy: &dropNewest,
		}),
	)

	// probe: read codec/container info. Deletes the file and returns IsValidMedia=false
	// (without error) when the file is not a recognisable media file or has no video stream,
	// which causes all downstream steps to be skipped via WithSkipIf.
	probeTask := wf.NewTask("probe", func(ctx hatchet.Context, input MovieInput) (probeOutput, error) {
		info, err := ffprobe.Probe(ctx, input.FilePath)
		if err != nil {
			_ = os.Remove(input.FilePath)
			return probeOutput{IsValidMedia: false}, nil
		}

		for _, s := range info.Streams {
			if s.CodecType == ffprobe.CodecTypeVideo {
				return probeOutput{
					IsValidMedia: true,
					CopyVideo:    selectVideoCodec(info) == ffmpeg.CodecCopy,
				}, nil
			}
		}

		// No video stream found — not a movie file.
		_ = os.Remove(input.FilePath)
		return probeOutput{IsValidMedia: false}, nil
	})

	skipIfInvalid := hatchet.WithSkipIf(hatchet.ParentCondition(probeTask, "output.is_valid_media == false"))

	// lookup: identify the movie in Radarr; fails with ErrNotFound if unrecognised.
	lookupTask := wf.NewTask("lookup", func(ctx hatchet.Context, input MovieInput) (lookupOutput, error) {
		movie, err := radarrClient.GetMovieByFilePath(ctx, input.FilePath)
		if err != nil {
			return lookupOutput{}, fmt.Errorf("lookup movie: %w", err)
		}
		return lookupOutput{MovieID: movie.ID}, nil
	}, hatchet.WithParents(probeTask), skipIfInvalid)

	// transcode: copy or re-encode the video stream, writing output to a temp path.
	transcodeTask := wf.NewTask("transcode", func(ctx hatchet.Context, input MovieInput) (transcodeOutput, error) {
		var probe probeOutput
		if err := ctx.ParentOutput(probeTask, &probe); err != nil {
			return transcodeOutput{}, fmt.Errorf("get probe output: %w", err)
		}

		videoCodec := ffmpeg.CodecH265
		if probe.CopyVideo {
			videoCodec = ffmpeg.CodecCopy
		}

		tmpFile, err := os.CreateTemp(cfg.OutputDir, ".tmp-*.mkv")
		if err != nil {
			return transcodeOutput{}, fmt.Errorf("create temp file: %w", err)
		}
		tempPath := tmpFile.Name()
		if err := tmpFile.Close(); err != nil {
			return transcodeOutput{}, fmt.Errorf("close temp file: %w", err)
		}

		if err := ffmpeg.NewTranscode(input.FilePath, tempPath).
			ToVideoCodec(videoCodec).
			ToContainer(ffmpeg.ContainerMKV).
			Build().
			Run(ctx); err != nil {
			_ = os.Remove(tempPath)
			return transcodeOutput{}, fmt.Errorf("transcode: %w", err)
		}

		return transcodeOutput{TempPath: tempPath}, nil
	}, hatchet.WithParents(probeTask, lookupTask), skipIfInvalid)

	// move: atomically rename the temp file to the final output path.
	moveTask := wf.NewTask("move", func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		var tc transcodeOutput
		if err := ctx.ParentOutput(transcodeTask, &tc); err != nil {
			return struct{}{}, fmt.Errorf("get transcode output: %w", err)
		}

		finalPath := filepath.Join(cfg.OutputDir, filepath.Base(input.FilePath))
		if err := os.Rename(tc.TempPath, finalPath); err != nil {
			_ = os.Remove(tc.TempPath)
			return struct{}{}, fmt.Errorf("move output file: %w", err)
		}

		return struct{}{}, nil
	}, hatchet.WithParents(probeTask, transcodeTask), skipIfInvalid)

	// notify: trigger a Radarr library rescan for the movie.
	notifyTask := wf.NewTask("notify", func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		var lu lookupOutput
		if err := ctx.ParentOutput(lookupTask, &lu); err != nil {
			return struct{}{}, fmt.Errorf("get lookup output: %w", err)
		}

		if err := radarrClient.RefreshMovie(ctx, lu.MovieID); err != nil {
			return struct{}{}, fmt.Errorf("notify radarr: %w", err)
		}

		return struct{}{}, nil
	}, hatchet.WithParents(probeTask, lookupTask, moveTask), skipIfInvalid)

	// cleanup: delete the original source file after successful processing.
	_ = wf.NewTask("cleanup", func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		if err := os.Remove(input.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return struct{}{}, fmt.Errorf("delete source file: %w", err)
		}
		return struct{}{}, nil
	}, hatchet.WithParents(probeTask, notifyTask), skipIfInvalid)

	// OnFailure: send a failure notification to the configured webhook.
	wf.OnFailure(func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		for stepName, errMsg := range ctx.StepRunErrors() {
			_ = webhookClient.NotifyFailure(ctx, webhook.FailureEvent{
				Workflow: movieWorkflowName,
				FilePath: input.FilePath,
				Step:     stepName,
				Err:      errors.New(errMsg),
			})
		}
		return struct{}{}, nil
	})

	return wf
}
