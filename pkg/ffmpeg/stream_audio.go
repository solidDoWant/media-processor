package ffmpeg

import (
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

// audioStreamState decodes and re-encodes an audio stream.
type audioStreamState struct {
	copyStreamState

	// Decoder state.
	dec streamDecoder

	// Encoder state.
	encCodec        *astiav.Codec
	encCodecContext *astiav.CodecContext
	encPkt          *astiav.Packet
	swrCtx          *astiav.SoftwareResampleContext
	audioFrame      *astiav.Frame

	outputCodec Codec
}

func (ass *audioStreamState) encoderContext() *astiav.CodecContext { return ass.encCodecContext }

func (ass *audioStreamState) free() {
	ass.dec.free()
	if ass.encCodecContext != nil {
		ass.encCodecContext.Free()
	}
	if ass.encPkt != nil {
		ass.encPkt.Free()
	}
	if ass.swrCtx != nil {
		ass.swrCtx.Free()
	}
	if ass.audioFrame != nil {
		ass.audioFrame.Free()
	}
}

// setupDecoder initialises the software decoder codec context for the audio stream.
func (ass *audioStreamState) setupDecoder(inStream *astiav.Stream) error {
	codec := astiav.FindDecoder(inStream.CodecParameters().CodecID())
	if codec == nil {
		return fmt.Errorf("no decoder for codec ID %v", inStream.CodecParameters().CodecID())
	}
	ass.dec.codec = codec

	ass.dec.codecContext = astiav.AllocCodecContext(codec)
	if ass.dec.codecContext == nil {
		return errors.New("failed to allocate decoder codec context")
	}

	if err := inStream.CodecParameters().ToCodecContext(ass.dec.codecContext); err != nil {
		return fmt.Errorf("copying codec parameters to context: %w", err)
	}

	if err := ass.dec.codecContext.Open(codec, nil); err != nil {
		return fmt.Errorf("opening decoder: %w", err)
	}
	ass.dec.codecContext.SetTimeBase(inStream.TimeBase())

	ass.dec.frame = astiav.AllocFrame()
	if ass.dec.frame == nil {
		return errors.New("failed to allocate decoder frame")
	}

	return nil
}

// setupEncoder implements the stream interface for audio.
func (ass *audioStreamState) setupEncoder(_ HWAccel, outputFmt *astiav.FormatContext) error {
	switch ass.outputCodec {
	case CodecH264, CodecH265:
		return fmt.Errorf("unsupported audio codec: %v", ass.outputCodec)
	}

	enc := astiav.FindEncoder(ass.outputCodec)
	if enc == nil {
		return fmt.Errorf("no encoder found for audio codec %v", ass.outputCodec)
	}
	ass.encCodec = enc

	ass.encCodecContext = astiav.AllocCodecContext(enc)
	if ass.encCodecContext == nil {
		return errors.New("failed to allocate audio encoder codec context")
	}

	// Preserve sample rate and channel layout.
	ass.encCodecContext.SetSampleRate(ass.dec.codecContext.SampleRate())

	// Prefer the decoder's channel layout if the encoder supports it;
	// fall back to the encoder's first supported layout otherwise.
	channelLayout := ass.dec.codecContext.ChannelLayout()
	if layouts := enc.SupportedChannelLayouts(); len(layouts) > 0 {
		supported := false
		for _, l := range layouts {
			if l.Channels() == channelLayout.Channels() {
				supported = true
				break
			}
		}
		if !supported {
			channelLayout = layouts[0]
		}
	}
	ass.encCodecContext.SetChannelLayout(channelLayout)

	sampleFmt := ass.dec.codecContext.SampleFormat()
	if fmts := enc.SupportedSampleFormats(); len(fmts) > 0 {
		sampleFmt = fmts[0]
	}
	ass.encCodecContext.SetSampleFormat(sampleFmt)

	ass.encCodecContext.SetTimeBase(astiav.NewRational(1, ass.encCodecContext.SampleRate()))

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		ass.encCodecContext.SetFlags(ass.encCodecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := ass.encCodecContext.Open(ass.encCodec, nil); err != nil {
		return fmt.Errorf("opening audio encoder: %w", err)
	}

	// Set up resampler if sample format, channel layout, or sample rate differs.
	needResample := ass.dec.codecContext.SampleFormat() != ass.encCodecContext.SampleFormat() ||
		ass.dec.codecContext.ChannelLayout().Channels() != ass.encCodecContext.ChannelLayout().Channels() ||
		ass.dec.codecContext.SampleRate() != ass.encCodecContext.SampleRate()

	if needResample {
		ass.swrCtx = astiav.AllocSoftwareResampleContext()
		if ass.swrCtx == nil {
			return errors.New("failed to allocate software resample context")
		}

		ass.audioFrame = astiav.AllocFrame()
		if ass.audioFrame == nil {
			return errors.New("failed to allocate audio resample frame")
		}
		ass.audioFrame.SetChannelLayout(ass.encCodecContext.ChannelLayout())
		ass.audioFrame.SetSampleFormat(ass.encCodecContext.SampleFormat())
		ass.audioFrame.SetSampleRate(ass.encCodecContext.SampleRate())
		ass.audioFrame.SetNbSamples(ass.dec.codecContext.FrameSize())
		if ass.audioFrame.NbSamples() <= 0 {
			ass.audioFrame.SetNbSamples(1024)
		}
		if err := ass.audioFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating audio resample frame buffer: %w", err)
		}
	}

	ass.encPkt = astiav.AllocPacket()
	if ass.encPkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// processPacket implements the stream interface for audio. It decodes the
// packet and re-encodes each decoded frame.
func (ass *audioStreamState) processPacket(packet *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	packet.RescaleTs(ass.inStream.TimeBase(), ass.dec.codecContext.TimeBase())

	if err := ass.dec.codecContext.SendPacket(packet); err != nil {
		return fmt.Errorf("ffmpeg: sending audio packet to decoder: %w", err)
	}

	for {
		if err := ass.dec.codecContext.ReceiveFrame(ass.dec.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving decoded audio frame: %w", err)
		}
		if err := ass.encodeAudioFrame(ass.dec.frame, outputFmt, progressCh, totalDuration); err != nil {
			ass.dec.frame.Unref()
			return err
		}
		ass.dec.frame.Unref()
	}
}

// encodeAudioFrame resamples (if needed) and encodes a single decoded audio frame.
func (ass *audioStreamState) encodeAudioFrame(frame *astiav.Frame, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	encFrame := frame

	if ass.swrCtx != nil {
		if err := ass.swrCtx.ConvertFrame(frame, ass.audioFrame); err != nil {
			return fmt.Errorf("ffmpeg: resampling audio frame: %w", err)
		}
		ass.audioFrame.SetPts(frame.Pts())
		encFrame = ass.audioFrame
	}

	if err := ass.encCodecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending audio frame to encoder: %w", err)
	}

	return ass.receiveAndWritePackets(ass.encCodecContext, ass.encPkt, outputFmt, progressCh, totalDuration)
}

// flush implements the stream interface for audio. It drains buffered frames
// from the encoder.
func (ass *audioStreamState) flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	if err := ass.encCodecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing audio encoder: %w", err)
	}
	return ass.receiveAndWritePackets(ass.encCodecContext, ass.encPkt, outputFmt, progressCh, totalDuration)
}
