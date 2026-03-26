package media

import (
	"context"
	"fmt"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// lookupOutput is the output of the lookup step.
type lookupOutput struct {
	MediaID int64 `json:"media_id"`
}

// runLookup identifies the media file at filePath using library and returns the
// ID needed for a subsequent library refresh. It returns an error wrapping
// medialib.ErrNotFound if the file is not recognised.
func runLookup(ctx context.Context, filePath string, library medialib.LibraryClient) (lookupOutput, error) {
	id, err := library.GetIDByFilePath(ctx, filePath)
	if err != nil {
		return lookupOutput{}, fmt.Errorf("lookup media: %w", err)
	}
	return lookupOutput{MediaID: id}, nil
}
