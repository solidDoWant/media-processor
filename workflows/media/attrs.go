package media

import (
	"strconv"

	"go.opentelemetry.io/otel/attribute"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/workflows/shared"
)

func mediaTypeAttr(mt medialib.MediaType) attribute.KeyValue {
	return attribute.String("media_type", string(mt))
}

func mappingNameAttr(name string) attribute.KeyValue {
	return attribute.String("mapping_name", name)
}

// buildStandardAttrs returns the full standard label set for processing metrics.
func buildStandardAttrs(input MediaInput, probe shared.ProbeOutput, transcode shared.TranscodeOutput, hardwareAccelerated bool) []attribute.KeyValue {
	hw := "false"
	if hardwareAccelerated {
		hw = "true"
	}

	return []attribute.KeyValue{
		attribute.String("source_codec", probe.VideoCodec),
		attribute.String("destination_codec", transcode.DestCodec),
		attribute.String("source_container", probe.Format),
		attribute.String("destination_container", transcode.DestContainer),
		mediaTypeAttr(input.MediaType),
		mappingNameAttr(input.MappingName),
		attribute.String("hardware_accelerated", hw),
	}
}

// highCardinalityAttrs returns high-cardinality per-item labels for a media item.
// Episode-specific labels (series_title, season_number, episode_number) are only
// added when mediaType is ShowType.
func highCardinalityAttrs(mediaType medialib.MediaType, info medialib.MediaInfo) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("id", strconv.FormatInt(info.GetID(), 10)),
		attribute.String("title", info.GetTitle()),
		attribute.String("year", strconv.Itoa(info.GetYear())),
	}
	if mediaType == medialib.ShowType {
		attrs = append(attrs,
			attribute.String("series_title", info.GetSeriesTitle()),
			attribute.String("season_number", strconv.Itoa(info.GetSeasonNumber())),
			attribute.String("episode_number", strconv.Itoa(info.GetEpisodeNumber())),
		)
	}

	return attrs
}
