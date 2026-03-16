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

func (s *audioStreamState) encoderContext() *astiav.CodecContext { return s.encCodecContext }

func (s *audioStreamState) free() {
	s.dec.free()
	if s.encCodecContext != nil {
		s.encCodecContext.Free()
	}
	if s.encPkt != nil {
		s.encPkt.Free()
	}
	if s.swrCtx != nil {
		s.swrCtx.Free()
	}
	if s.audioFrame != nil {
		s.audioFrame.Free()
	}
}

// setupDecoder initialises the software decoder codec context for the audio stream.
func (s *audioStreamState) setupDecoder(inStream *astiav.Stream) error {
	codec := astiav.FindDecoder(inStream.CodecParameters().CodecID())
	if codec == nil {
		return fmt.Errorf("no decoder for codec ID %v", inStream.CodecParameters().CodecID())
	}
	s.dec.codec = codec

	s.dec.codecContext = astiav.AllocCodecContext(codec)
	if s.dec.codecContext == nil {
		return errors.New("failed to allocate decoder codec context")
	}

	if err := inStream.CodecParameters().ToCodecContext(s.dec.codecContext); err != nil {
		return fmt.Errorf("copying codec parameters to context: %w", err)
	}

	if err := s.dec.codecContext.Open(codec, nil); err != nil {
		return fmt.Errorf("opening decoder: %w", err)
	}
	s.dec.codecContext.SetTimeBase(inStream.TimeBase())

	s.dec.frame = astiav.AllocFrame()
	if s.dec.frame == nil {
		return errors.New("failed to allocate decoder frame")
	}

	return nil
}

// setupEncoder implements the stream interface for audio.
func (s *audioStreamState) setupEncoder(_ HWAccel, outputFmt *astiav.FormatContext) error {
	switch s.outputCodec {
	case CodecH264, CodecH265:
		return fmt.Errorf("unsupported audio codec: %s", s.outputCodec)
	}

	// Re-encode using the same codec as the input (transcode → same format,
	// potentially with a different container).
	enc := astiav.FindEncoder(s.dec.codecContext.CodecID())
	if enc == nil {
		return fmt.Errorf("no encoder found for audio codec ID %v", s.dec.codecContext.CodecID())
	}
	s.encCodec = enc

	s.encCodecContext = astiav.AllocCodecContext(enc)
	if s.encCodecContext == nil {
		return errors.New("failed to allocate audio encoder codec context")
	}

	// Preserve sample rate and channel layout.
	s.encCodecContext.SetSampleRate(s.dec.codecContext.SampleRate())

	channelLayout := s.dec.codecContext.ChannelLayout()
	if layouts := enc.SupportedChannelLayouts(); len(layouts) > 0 {
		channelLayout = layouts[0]
	}
	s.encCodecContext.SetChannelLayout(channelLayout)

	sampleFmt := s.dec.codecContext.SampleFormat()
	if fmts := enc.SupportedSampleFormats(); len(fmts) > 0 {
		sampleFmt = fmts[0]
	}
	s.encCodecContext.SetSampleFormat(sampleFmt)

	s.encCodecContext.SetTimeBase(astiav.NewRational(1, s.encCodecContext.SampleRate()))

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		s.encCodecContext.SetFlags(s.encCodecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := s.encCodecContext.Open(s.encCodec, nil); err != nil {
		return fmt.Errorf("opening audio encoder: %w", err)
	}

	// Set up resampler if sample format, channel layout, or sample rate differs.
	needResample := s.dec.codecContext.SampleFormat() != s.encCodecContext.SampleFormat() ||
		s.dec.codecContext.ChannelLayout().Channels() != s.encCodecContext.ChannelLayout().Channels() ||
		s.dec.codecContext.SampleRate() != s.encCodecContext.SampleRate()

	if needResample {
		s.swrCtx = astiav.AllocSoftwareResampleContext()
		if s.swrCtx == nil {
			return errors.New("failed to allocate software resample context")
		}

		s.audioFrame = astiav.AllocFrame()
		if s.audioFrame == nil {
			return errors.New("failed to allocate audio resample frame")
		}
		s.audioFrame.SetChannelLayout(s.encCodecContext.ChannelLayout())
		s.audioFrame.SetSampleFormat(s.encCodecContext.SampleFormat())
		s.audioFrame.SetSampleRate(s.encCodecContext.SampleRate())
		s.audioFrame.SetNbSamples(s.dec.codecContext.FrameSize())
		if s.audioFrame.NbSamples() <= 0 {
			s.audioFrame.SetNbSamples(1024)
		}
		if err := s.audioFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating audio resample frame buffer: %w", err)
		}
	}

	s.encPkt = astiav.AllocPacket()
	if s.encPkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// processPacket implements the stream interface for audio. It decodes the
// packet and re-encodes each decoded frame.
func (s *audioStreamState) processPacket(pkt *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	pkt.RescaleTs(s.inStream.TimeBase(), s.dec.codecContext.TimeBase())

	if err := s.dec.codecContext.SendPacket(pkt); err != nil {
		return fmt.Errorf("ffmpeg: sending audio packet to decoder: %w", err)
	}

	for {
		if err := s.dec.codecContext.ReceiveFrame(s.dec.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving decoded audio frame: %w", err)
		}
		if err := s.encodeAudioFrame(s.dec.frame, outputFmt, progressCh, totalDuration); err != nil {
			s.dec.frame.Unref()
			return err
		}
		s.dec.frame.Unref()
	}
}

// encodeAudioFrame resamples (if needed) and encodes a single decoded audio frame.
func (s *audioStreamState) encodeAudioFrame(frame *astiav.Frame, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	encFrame := frame

	if s.swrCtx != nil {
		if err := s.swrCtx.ConvertFrame(frame, s.audioFrame); err != nil {
			return fmt.Errorf("ffmpeg: resampling audio frame: %w", err)
		}
		s.audioFrame.SetPts(frame.Pts())
		encFrame = s.audioFrame
	}

	if err := s.encCodecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending audio frame to encoder: %w", err)
	}

	return s.receiveAndWritePackets(s.encCodecContext, s.encPkt, outputFmt, progressCh, totalDuration)
}

// flush implements the stream interface for audio. It drains buffered frames
// from the encoder.
func (s *audioStreamState) flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	if err := s.encCodecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing audio encoder: %w", err)
	}
	return s.receiveAndWritePackets(s.encCodecContext, s.encPkt, outputFmt, progressCh, totalDuration)
}
