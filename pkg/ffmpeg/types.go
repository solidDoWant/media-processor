package ffmpeg

import "github.com/asticode/go-astiav"

// Codec is an alias for astiav.CodecID, identifying a video or audio codec.
// Using a type alias keeps Codec values directly usable in libavcodec calls
// without explicit conversions.
type Codec = astiav.CodecID

const (
	// CodecH265 encodes video as H.265/HEVC (libx265 for software, or hardware variant).
	CodecH265 Codec = astiav.CodecIDH265
	// CodecCopy copies the stream without re-encoding.
	CodecCopy Codec = astiav.CodecIDNone
)

// Container identifies an output container format.
type Container string

const (
	// ContainerMKV produces a Matroska (.mkv) container.
	ContainerMKV Container = "matroska"
	// ContainerMP4 produces an MP4 container.
	ContainerMP4 Container = "mp4"
)

// HWAccel selects hardware acceleration for encoding.
type HWAccel int

const (
	// HWAccelNone uses software encoding only.
	HWAccelNone HWAccel = iota
	// HWAccelVAAPI uses VAAPI.
	HWAccelVAAPI
	// HWAccelQSV uses Intel Quick Sync Video.
	HWAccelQSV
	// HWAccelAuto detects available hardware at runtime and falls back to software.
	HWAccelAuto
)

// CropParams describes the non-black region detected by the cropdetect filter.
// All values are in pixels.
type CropParams struct {
	W, H int
	X, Y int
}

// Progress is a periodic update emitted during transcoding.
type Progress struct {
	// FramesProcessed is the number of encoded frames written so far.
	FramesProcessed int64
	// PercentComplete is the estimated completion percentage (0–100).
	PercentComplete float64
}
