package ffmpeg

import (
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

// stream is the interface for all per-stream processing types.
// copyStreamState, videoStreamState, and audioStreamState implement it.
type stream interface {
	inputStream() *astiav.Stream
	outputStream() *astiav.Stream
	setOutputStream(*astiav.Stream)
	// setupEncoder configures the encoder and allocates encoder resources.
	// For copy streams this is a no-op.
	setupEncoder(hwAccel HWAccel, outputFmt *astiav.FormatContext) error
	// encoderContext returns the encoder codec context used to populate output
	// stream parameters. Returns nil for copy streams.
	encoderContext() *astiav.CodecContext
	// processPacket handles an incoming demuxed packet.
	processPacket(pkt *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error
	// flush drains any buffered encoder output. No-op for copy streams.
	flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error
	free()
}

// copyStreamState passes packets through to the output without re-encoding.
// Used for subtitle, attachment, data, and copy-codec audio/video streams.
// It is also embedded by videoStreamState and audioStreamState to provide the
// common inStream/outStream fields and the encoded-frame counter.
type copyStreamState struct {
	inStream  *astiav.Stream
	outStream *astiav.Stream
	frames    int64 // encoded frames written; used for progress reporting
}

func (s *copyStreamState) inputStream() *astiav.Stream        { return s.inStream }
func (s *copyStreamState) outputStream() *astiav.Stream       { return s.outStream }
func (s *copyStreamState) setOutputStream(out *astiav.Stream) { s.outStream = out }

func (s *copyStreamState) setupEncoder(_ HWAccel, _ *astiav.FormatContext) error { return nil }
func (s *copyStreamState) encoderContext() *astiav.CodecContext                  { return nil }

func (s *copyStreamState) processPacket(pkt *astiav.Packet, outputFmt *astiav.FormatContext, _ chan<- Progress, _ int64) error {
	return remuxPacket(pkt, s.inStream, s.outStream, outputFmt)
}

func (s *copyStreamState) flush(_ *astiav.FormatContext, _ chan<- Progress, _ int64) error {
	return nil
}

func (s *copyStreamState) free() {}

// receiveAndWritePackets drains encoded packets from the encoder context and
// writes each to the output. s.frames is incremented per written packet.
func (s *copyStreamState) receiveAndWritePackets(encCtx *astiav.CodecContext, encPkt *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	for {
		if err := encCtx.ReceivePacket(encPkt); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving encoded packet: %w", err)
		}

		s.frames++
		encPkt.SetStreamIndex(s.outStream.Index())
		encPkt.RescaleTs(encCtx.TimeBase(), s.outStream.TimeBase())

		if progressCh != nil {
			sendProgress(progressCh, s.frames, encPkt, s.outStream, totalDuration)
		}

		if err := outputFmt.WriteInterleavedFrame(encPkt); err != nil {
			encPkt.Unref()
			return fmt.Errorf("ffmpeg: writing encoded packet: %w", err)
		}
		encPkt.Unref()
	}
}

// remuxPacket copies a packet directly to the output without decoding/encoding.
func remuxPacket(pkt *astiav.Packet, inStream, outStream *astiav.Stream, outputFmt *astiav.FormatContext) error {
	pkt.RescaleTs(inStream.TimeBase(), outStream.TimeBase())
	pkt.SetStreamIndex(outStream.Index())
	if err := outputFmt.WriteInterleavedFrame(pkt); err != nil {
		return fmt.Errorf("ffmpeg: writing remuxed packet for stream %d: %w", outStream.Index(), err)
	}
	return nil
}

// sendProgress emits a non-blocking progress update on ch.
func sendProgress(ch chan<- Progress, frames int64, pkt *astiav.Packet, outStream *astiav.Stream, totalDuration int64) {
	var pct float64
	if totalDuration > 0 {
		tb := outStream.TimeBase()
		ptsInMicros := float64(pkt.Pts()) * float64(tb.Num()) / float64(tb.Den()) * 1e6
		pct = ptsInMicros / float64(totalDuration) * 100
		if pct > 100 {
			pct = 100
		}
		if pct < 0 {
			pct = 0
		}
	}
	select {
	case ch <- Progress{FramesProcessed: frames, PercentComplete: pct}:
	default:
	}
}

// freeStreams releases all resources held by a streams map.
func freeStreams(streams map[int]stream) {
	for _, s := range streams {
		s.free()
	}
}
