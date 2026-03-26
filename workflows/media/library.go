package media

import (
	"fmt"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// selectLibrary returns the LibraryClient corresponding to mediaType, using
// radarrClient for movies and sonarrClient for TV episodes. It is the single
// dispatch point for media-type selection in the workflow.
func selectLibrary(mediaType medialib.MediaType, radarrClient, sonarrClient medialib.Library) (medialib.Library, error) {
	switch mediaType {
	case medialib.MovieType:
		return radarrClient, nil
	case medialib.ShowType:
		return sonarrClient, nil
	default:
		return nil, fmt.Errorf("unknown media type %q", mediaType)
	}
}
