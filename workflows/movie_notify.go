package workflows

import (
	"context"
	"fmt"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// runNotify triggers a Radarr library rescan for the movie identified by lu.
func runNotify(ctx context.Context, lu lookupOutput, radarrClient medialib.MovieLibrary) error {
	if err := radarrClient.RefreshMovie(ctx, lu.MovieID); err != nil {
		return fmt.Errorf("notify radarr: %w", err)
	}
	return nil
}
