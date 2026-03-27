package ffmpeg

import "github.com/asticode/go-astiav"

// Codec is an alias for astiav.CodecID, identifying a video or audio codec.
// Using a type alias keeps Codec values directly usable in libavcodec calls
// without explicit conversions.
type Codec = astiav.CodecID

const (
	// CodecH264 encodes video as H.264 (libx264 for software, or hardware variant).
	CodecH264 Codec = astiav.CodecIDH264
	// CodecH265 encodes video as H.265/HEVC (libx265 for software, or hardware variant).
	CodecH265 Codec = astiav.CodecIDH265
	// CodecAC3 encodes audio as Dolby AC-3.
	CodecAC3 Codec = astiav.CodecIDAc3
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
	// HWAccelNVENC uses NVIDIA NVENC via CUDA.
	HWAccelNVENC
	// HWAccelVAAPI uses Intel/AMD VAAPI.
	HWAccelVAAPI
	// HWAccelQSV uses Intel Quick Sync Video.
	HWAccelQSV
	// HWAccelAuto detects available hardware at runtime and falls back to software.
	HWAccelAuto
)

// Progress is a periodic update emitted during transcoding.
type Progress struct {
	// FramesProcessed is the number of encoded frames written so far.
	FramesProcessed int64
	// PercentComplete is the estimated completion percentage (0–100).
	PercentComplete float64
}
