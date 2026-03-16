package ffmpeg

// Codec identifies an output video or audio codec.
type Codec string

const (
	// CodecH264 encodes video as H.264 (libx264 for software, or hardware variant).
	CodecH264 Codec = "h264"
	// CodecH265 encodes video as H.265/HEVC (libx265 for software, or hardware variant).
	CodecH265 Codec = "hevc"
	// CodecCopy copies the stream without re-encoding.
	CodecCopy Codec = "copy"
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
