package ffmpeg

import (
	"errors"
	"fmt"
	"slices"

	"github.com/asticode/go-astiav"
)

type audioEncoderState struct {
	codecID         astiav.CodecID
	codec           *astiav.Codec
	codecContext    *astiav.CodecContext
	packet          *astiav.Packet
	resampleContext *astiav.SoftwareResampleContext
	frame           *astiav.Frame
}

// audioStreamState decodes and re-encodes an audio stream.
type audioStreamState struct {
	copyStreamState
	decoder streamDecoderState
	encoder audioEncoderState
}

func (ass *audioStreamState) encoderContext() *astiav.CodecContext { return ass.encoder.codecContext }

func (ass *audioStreamState) free() {
	ass.decoder.free()

	if ass.encoder.codecContext != nil {
		ass.encoder.codecContext.Free()
	}

	if ass.encoder.packet != nil {
		ass.encoder.packet.Free()
	}

	if ass.encoder.resampleContext != nil {
		ass.encoder.resampleContext.Free()
	}

	if ass.encoder.frame != nil {
		ass.encoder.frame.Free()
	}
}

// setupDecoder initialises the software decoder codec context for the audio stream.
func (ass *audioStreamState) setupDecoder(inStream *astiav.Stream) error {
	ass.decoder.codec = astiav.FindDecoder(inStream.CodecParameters().CodecID())
	if ass.decoder.codec == nil {
		return fmt.Errorf("no decoder for codec ID %v", inStream.CodecParameters().CodecID())
	}

	ass.decoder.codecContext = astiav.AllocCodecContext(ass.decoder.codec)
	if ass.decoder.codecContext == nil {
		return errors.New("failed to allocate decoder codec context")
	}

	if err := inStream.CodecParameters().ToCodecContext(ass.decoder.codecContext); err != nil {
		return fmt.Errorf("copying codec parameters to context: %w", err)
	}

	if err := ass.decoder.codecContext.Open(ass.decoder.codec, nil); err != nil {
		return fmt.Errorf("opening decoder: %w", err)
	}
	ass.decoder.codecContext.SetTimeBase(inStream.TimeBase())

	ass.decoder.frame = astiav.AllocFrame()
	if ass.decoder.frame == nil {
		return errors.New("failed to allocate decoder frame")
	}

	return nil
}

// setupEncoder implements the stream interface for audio.
func (ass *audioStreamState) setupEncoder(_ HWAccel, outputFmt *astiav.FormatContext) error {
	ass.encoder.codec = astiav.FindEncoder(ass.encoder.codecID)
	if ass.encoder.codec == nil {
		return fmt.Errorf("no encoder found for audio codec %v", ass.encoder.codecID)
	}

	ass.encoder.codecContext = astiav.AllocCodecContext(ass.encoder.codec)
	if ass.encoder.codecContext == nil {
		return errors.New("failed to allocate audio encoder codec context")
	}

	// Preserve sample rate and channel layout.
	ass.encoder.codecContext.SetSampleRate(ass.decoder.codecContext.SampleRate())

	// Prefer the decoder's channel layout if the encoder supports it exactly;
	// fall back to the encoder's first supported layout otherwise.
	channelLayout := ass.decoder.codecContext.ChannelLayout()
	if layouts := ass.encoder.codec.SupportedChannelLayouts(); len(layouts) > 0 {
		supported := slices.ContainsFunc(layouts, func(layout astiav.ChannelLayout) bool {
			return layout.Equal(channelLayout)
		})

		if !supported {
			channelLayout = layouts[0]
		}
	}
	ass.encoder.codecContext.SetChannelLayout(channelLayout)

	// Prefer the decoder's sample format if the encoder supports it;
	// fall back to the encoder's first supported format otherwise.
	sampleFormat := ass.decoder.codecContext.SampleFormat()
	if fmts := ass.encoder.codec.SupportedSampleFormats(); len(fmts) > 0 {
		supported := slices.Contains(fmts, sampleFormat)

		if !supported {
			sampleFormat = fmts[0]
		}
	}
	ass.encoder.codecContext.SetSampleFormat(sampleFormat)

	ass.encoder.codecContext.SetTimeBase(astiav.NewRational(1, ass.encoder.codecContext.SampleRate()))

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		ass.encoder.codecContext.SetFlags(ass.encoder.codecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := ass.encoder.codecContext.Open(ass.encoder.codec, nil); err != nil {
		return fmt.Errorf("opening audio encoder: %w", err)
	}

	// Set up resampler if sample format, channel layout, or sample rate differs.
	needResample := ass.decoder.codecContext.SampleFormat() != ass.encoder.codecContext.SampleFormat() ||
		!ass.decoder.codecContext.ChannelLayout().Equal(ass.encoder.codecContext.ChannelLayout()) ||
		ass.decoder.codecContext.SampleRate() != ass.encoder.codecContext.SampleRate()

	if needResample {
		ass.encoder.resampleContext = astiav.AllocSoftwareResampleContext()
		if ass.encoder.resampleContext == nil {
			return errors.New("failed to allocate software resample context")
		}

		ass.encoder.frame = astiav.AllocFrame()
		if ass.encoder.frame == nil {
			return errors.New("failed to allocate audio resample frame")
		}

		ass.encoder.frame.SetChannelLayout(ass.encoder.codecContext.ChannelLayout())
		ass.encoder.frame.SetSampleFormat(ass.encoder.codecContext.SampleFormat())
		ass.encoder.frame.SetSampleRate(ass.encoder.codecContext.SampleRate())
		ass.encoder.frame.SetNbSamples(ass.decoder.codecContext.FrameSize())

		if ass.encoder.frame.NbSamples() <= 0 {
			ass.encoder.frame.SetNbSamples(1024)
		}

		if err := ass.encoder.frame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating audio resample frame buffer: %w", err)
		}
	}

	ass.encoder.packet = astiav.AllocPacket()
	if ass.encoder.packet == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// processPacket implements the stream interface for audio. It decodes the
// packet and re-encodes each decoded frame.
func (ass *audioStreamState) processPacket(packet *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	packet.RescaleTs(ass.inStream.TimeBase(), ass.decoder.codecContext.TimeBase())

	if err := ass.decoder.codecContext.SendPacket(packet); err != nil {
		return fmt.Errorf("ffmpeg: sending audio packet to decoder: %w", err)
	}

	for {
		if err := ass.decoder.codecContext.ReceiveFrame(ass.decoder.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}

			return fmt.Errorf("ffmpeg: receiving decoded audio frame: %w", err)
		}

		if err := ass.encodeAudioFrame(ass.decoder.frame, outputFmt, progressCh, totalDuration); err != nil {
			ass.decoder.frame.Unref()
			return err
		}

		ass.decoder.frame.Unref()
	}
}

// encodeAudioFrame resamples (if needed) and encodes a single decoded audio frame.
func (ass *audioStreamState) encodeAudioFrame(frame *astiav.Frame, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	encFrame := frame

	if ass.encoder.resampleContext != nil {
		if err := ass.encoder.resampleContext.ConvertFrame(frame, ass.encoder.frame); err != nil {
			return fmt.Errorf("ffmpeg: resampling audio frame: %w", err)
		}

		ass.encoder.frame.SetPts(frame.Pts())
		encFrame = ass.encoder.frame
	}

	if err := ass.encoder.codecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending audio frame to encoder: %w", err)
	}

	return ass.receiveAndWritePackets(ass.encoder.codecContext, ass.encoder.packet, outputFmt, progressCh, totalDuration)
}

// flush implements the stream interface for audio. It first drains any frames
// buffered inside the decoder, then flushes the encoder.
func (ass *audioStreamState) flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	// Signal EOF to the decoder so it releases all buffered frames.
	if err := ass.decoder.codecContext.SendPacket(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing audio decoder: %w", err)
	}

	for {
		if err := ass.decoder.codecContext.ReceiveFrame(ass.decoder.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				break
			}

			return fmt.Errorf("ffmpeg: receiving flushed audio frame: %w", err)
		}

		if err := ass.encodeAudioFrame(ass.decoder.frame, outputFmt, progressCh, totalDuration); err != nil {
			ass.decoder.frame.Unref()
			return err
		}

		ass.decoder.frame.Unref()
	}

	// Flush the encoder.
	if err := ass.encoder.codecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing audio encoder: %w", err)
	}

	return ass.receiveAndWritePackets(ass.encoder.codecContext, ass.encoder.packet, outputFmt, progressCh, totalDuration)
}
