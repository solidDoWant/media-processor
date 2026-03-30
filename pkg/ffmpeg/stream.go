package ffmpeg

import (
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

// stream is the interface for all per-stream processing types.
// copyStreamState, videoStreamState, and audioStreamState implement it.
type stream interface {
	// inputStream returns the source stream from the input format context.
	inputStream() *astiav.Stream
	// outputStream returns the destination stream in the output format context.
	outputStream() *astiav.Stream
	// setOutputStream binds the output stream produced by the muxer to this state.
	setOutputStream(*astiav.Stream)
	// setupEncoder configures the encoder and allocates encoder resources.
	// For copy streams this is a no-op.
	setupEncoder(hwAccel HWAccel, outputFmt *astiav.FormatContext) error
	// encoderContext returns the encoder codec context used to populate output
	// stream parameters. Returns nil for copy streams.
	encoderContext() *astiav.CodecContext
	// processPacket handles an incoming demuxed packet.
	processPacket(packet *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error
	// flush drains any buffered encoder output. No-op for copy streams.
	flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error
	// free releases all resources held by this stream state.
	free()
}

// streamDecoderState holds resources for the decoder side of a stream. Used by
// both videoStreamState and audioStreamState.
type streamDecoderState struct {
	codec        *astiav.Codec
	codecContext *astiav.CodecContext
	frame        *astiav.Frame
}

func (sd *streamDecoderState) free() {
	if sd.codecContext != nil {
		sd.codecContext.Free()
	}

	if sd.frame != nil {
		sd.frame.Free()
	}
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

func (css *copyStreamState) inputStream() *astiav.Stream        { return css.inStream }
func (css *copyStreamState) outputStream() *astiav.Stream       { return css.outStream }
func (css *copyStreamState) setOutputStream(out *astiav.Stream) { css.outStream = out }

func (css *copyStreamState) setupEncoder(_ HWAccel, _ *astiav.FormatContext) error { return nil }
func (css *copyStreamState) encoderContext() *astiav.CodecContext                  { return nil }

func (css *copyStreamState) processPacket(packet *astiav.Packet, outputFmt *astiav.FormatContext, _ chan<- Progress, _ int64) error {
	return remuxPacket(packet, css.inStream, css.outStream, outputFmt)
}

func (css *copyStreamState) flush(_ *astiav.FormatContext, _ chan<- Progress, _ int64) error {
	return nil
}

func (css *copyStreamState) free() {}

// receiveAndWritePackets drains encoded packets from the encoder context and
// writes each to the output. css.frames is incremented per written packet.
func (css *copyStreamState) receiveAndWritePackets(encCtx *astiav.CodecContext, encPkt *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	for {
		if err := encCtx.ReceivePacket(encPkt); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}

			return fmt.Errorf("ffmpeg: receiving encoded packet: %w", err)
		}

		css.frames++
		encPkt.SetStreamIndex(css.outStream.Index())
		encPkt.RescaleTs(encCtx.TimeBase(), css.outStream.TimeBase())

		if progressCh != nil {
			sendProgress(progressCh, css.frames, encPkt, css.outStream, totalDuration)
		}

		if err := outputFmt.WriteInterleavedFrame(encPkt); err != nil {
			encPkt.Unref()
			return fmt.Errorf("ffmpeg: writing encoded packet: %w", err)
		}

		encPkt.Unref()
	}
}

// remuxPacket copies a packet directly to the output without decoding/encoding.
func remuxPacket(packet *astiav.Packet, inStream, outStream *astiav.Stream, outputFmt *astiav.FormatContext) error {
	packet.RescaleTs(inStream.TimeBase(), outStream.TimeBase())
	packet.SetStreamIndex(outStream.Index())

	if err := outputFmt.WriteInterleavedFrame(packet); err != nil {
		return fmt.Errorf("ffmpeg: writing remuxed packet for stream %d: %w", outStream.Index(), err)
	}

	return nil
}

// sendProgress emits a non-blocking progress update on ch.
func sendProgress(ch chan<- Progress, frames int64, packet *astiav.Packet, outStream *astiav.Stream, totalDuration int64) {
	var percentComplete float64

	if totalDuration > 0 {
		tb := outStream.TimeBase()
		ptsInMicros := float64(packet.Pts()) * float64(tb.Num()) / float64(tb.Den()) * 1e6
		percentComplete = ptsInMicros / float64(totalDuration) * 100

		if percentComplete > 100 {
			percentComplete = 100
		}

		if percentComplete < 0 {
			percentComplete = 0
		}
	}

	select {
	case ch <- Progress{FramesProcessed: frames, PercentComplete: percentComplete}:
	default:
	}
}

// freeStreams releases all resources held by a streams map.
func freeStreams(streams map[int]stream) {
	for _, s := range streams {
		s.free()
	}
}
