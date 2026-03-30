package ffmpeg

import (
	"errors"
	"fmt"
	"slices"

	"github.com/asticode/go-astiav"
)

type audioEncoderState struct {
	codecID             astiav.CodecID
	codec               *astiav.Codec
	codecContext        *astiav.CodecContext
	packet              *astiav.Packet
	resampleContext     *astiav.SoftwareResampleContext
	frame               *astiav.Frame
	targetChannelLayout *astiav.ChannelLayout // if set, overrides decoder layout selection
	// fifo and fifoFrame are used for fixed-frame-size encoders (e.g. AC-3) to
	// buffer resampled samples and drain them in exact encoder-frame-sized chunks.
	fifo               *astiav.AudioFifo
	fifoFrame          *astiav.Frame
	nextPts            int64 // running PTS counter for fixed-frame-size encoding
	nextPtsInitialized bool  // true after nextPts is seeded from the first source frame
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

	if ass.encoder.fifo != nil {
		ass.encoder.fifo.Free()
	}

	if ass.encoder.fifoFrame != nil {
		ass.encoder.fifoFrame.Free()
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

	// Select a sample rate supported by the encoder. Prefer the decoder's rate;
	// fall back to 48 kHz (a standard AC-3/AAC rate) then the first supported rate.
	sampleRate := ass.decoder.codecContext.SampleRate()
	if supported := ass.encoder.codec.SupportedSampleRates(); len(supported) > 0 {
		if !slices.Contains(supported, sampleRate) {
			if slices.Contains(supported, 48000) {
				sampleRate = 48000
			} else {
				sampleRate = supported[0]
			}
		}
	}

	ass.encoder.codecContext.SetSampleRate(sampleRate)

	// Determine channel layout: if a target is requested, prefer it; then the
	// decoder's layout; fall back to the encoder's first supported layout.
	channelLayout := ass.decoder.codecContext.ChannelLayout()
	if ass.encoder.targetChannelLayout != nil {
		channelLayout = *ass.encoder.targetChannelLayout
	}

	if layouts := ass.encoder.codec.SupportedChannelLayouts(); len(layouts) > 0 {
		supported := slices.ContainsFunc(layouts, func(layout astiav.ChannelLayout) bool {
			return layout.Equal(channelLayout)
		})
		if !supported {
			// If the requested target is not supported, try stereo as a fallback
			// before taking the encoder's first supported layout.
			if ass.encoder.targetChannelLayout != nil {
				stereo := astiav.ChannelLayoutStereo
				if slices.ContainsFunc(layouts, func(layout astiav.ChannelLayout) bool {
					return layout.Equal(stereo)
				}) {
					channelLayout = stereo
				} else {
					channelLayout = layouts[0]
				}
			} else {
				channelLayout = layouts[0]
			}
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

	// Set up FIFO for fixed-frame-size encoders (e.g. AC-3 requires 1536 samples)
	// regardless of whether resampling is needed. This ensures the encoder always
	// receives full-sized frames even when sample format/layout/rate already match.
	encoderFrameSize := ass.encoder.codecContext.FrameSize()
	if encoderFrameSize > 0 {
		ass.encoder.fifo = astiav.AllocAudioFifo(
			ass.encoder.codecContext.SampleFormat(),
			ass.encoder.codecContext.ChannelLayout().Channels(),
			encoderFrameSize,
		)
		if ass.encoder.fifo == nil {
			return errors.New("failed to allocate audio FIFO")
		}

		ass.encoder.fifoFrame = astiav.AllocFrame()
		if ass.encoder.fifoFrame == nil {
			return errors.New("failed to allocate audio FIFO frame")
		}

		ass.encoder.fifoFrame.SetChannelLayout(ass.encoder.codecContext.ChannelLayout())
		ass.encoder.fifoFrame.SetSampleFormat(ass.encoder.codecContext.SampleFormat())
		ass.encoder.fifoFrame.SetSampleRate(ass.encoder.codecContext.SampleRate())
		ass.encoder.fifoFrame.SetNbSamples(encoderFrameSize)

		if err := ass.encoder.fifoFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating audio FIFO frame buffer: %w", err)
		}
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

		if encoderFrameSize <= 0 {
			// Variable-frame-size encoder: pre-allocate the resample output frame.
			nbSamples := ass.decoder.codecContext.FrameSize()
			if nbSamples <= 0 {
				nbSamples = 1024
			}

			ass.encoder.frame.SetNbSamples(nbSamples)

			if err := ass.encoder.frame.AllocBuffer(0); err != nil {
				return fmt.Errorf("allocating audio resample frame buffer: %w", err)
			}
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
	if ass.encoder.resampleContext != nil {
		if err := ass.encoder.resampleContext.ConvertFrame(frame, ass.encoder.frame); err != nil {
			return fmt.Errorf("ffmpeg: resampling audio frame: %w", err)
		}
	}

	if ass.encoder.fifo != nil {
		// Seed the PTS counter from the first source frame so the downmix
		// track is aligned with the source timeline.
		if !ass.encoder.nextPtsInitialized && frame.Pts() != astiav.NoPtsValue {
			ass.encoder.nextPts = astiav.RescaleQ(
				frame.Pts(),
				ass.decoder.codecContext.TimeBase(),
				ass.encoder.codecContext.TimeBase(),
			)
			ass.encoder.nextPtsInitialized = true
		}

		// Use resampled output when available; otherwise buffer the decoded
		// frame directly (fixed-frame-size encoder, no resampling needed).
		fifoInput := frame
		if ass.encoder.resampleContext != nil {
			fifoInput = ass.encoder.frame
		}

		if _, err := ass.encoder.fifo.Write(fifoInput); err != nil {
			if ass.encoder.resampleContext != nil {
				fifoInput.Unref()
				ass.encoder.frame.SetChannelLayout(ass.encoder.codecContext.ChannelLayout())
				ass.encoder.frame.SetSampleFormat(ass.encoder.codecContext.SampleFormat())
				ass.encoder.frame.SetSampleRate(ass.encoder.codecContext.SampleRate())
			}

			return fmt.Errorf("ffmpeg: writing to audio FIFO: %w", err)
		}

		if ass.encoder.resampleContext != nil {
			// av_frame_unref resets format fields; restore them so the next
			// swr_convert_frame call sees the expected output parameters.
			ass.encoder.frame.Unref()
			ass.encoder.frame.SetChannelLayout(ass.encoder.codecContext.ChannelLayout())
			ass.encoder.frame.SetSampleFormat(ass.encoder.codecContext.SampleFormat())
			ass.encoder.frame.SetSampleRate(ass.encoder.codecContext.SampleRate())
		}

		return ass.drainFifo(outputFmt, progressCh, totalDuration)
	}

	encFrame := frame
	if ass.encoder.resampleContext != nil {
		ass.encoder.frame.SetPts(frame.Pts())
		encFrame = ass.encoder.frame
	}

	if err := ass.encoder.codecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending audio frame to encoder: %w", err)
	}

	return ass.receiveAndWritePackets(ass.encoder.codecContext, ass.encoder.packet, outputFmt, progressCh, totalDuration)
}

// drainFifo reads encoder-frame-sized chunks from the FIFO and encodes them
// until fewer than a full frame's worth of samples remain.
func (ass *audioStreamState) drainFifo(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	frameSize := ass.encoder.codecContext.FrameSize()
	for ass.encoder.fifo.Size() >= frameSize {
		ass.encoder.fifoFrame.SetPts(ass.encoder.nextPts)
		ass.encoder.nextPts += int64(frameSize)

		if err := ass.encoder.fifoFrame.MakeWritable(); err != nil {
			return fmt.Errorf("ffmpeg: making FIFO frame writable: %w", err)
		}

		if _, err := ass.encoder.fifo.Read(ass.encoder.fifoFrame); err != nil {
			return fmt.Errorf("ffmpeg: reading from audio FIFO: %w", err)
		}

		if err := ass.encoder.codecContext.SendFrame(ass.encoder.fifoFrame); err != nil {
			return fmt.Errorf("ffmpeg: sending audio frame to encoder: %w", err)
		}

		if err := ass.receiveAndWritePackets(ass.encoder.codecContext, ass.encoder.packet, outputFmt, progressCh, totalDuration); err != nil {
			return err
		}
	}

	return nil
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

	// Flush FIFO for fixed-frame-size encoders.
	if ass.encoder.fifo != nil {
		if ass.encoder.resampleContext != nil {
			// Flush any samples remaining in the resampler's internal delay buffer.
			if err := ass.encoder.resampleContext.ConvertFrame(nil, ass.encoder.frame); err != nil {
				ass.encoder.frame.Unref()

				if !errors.Is(err, astiav.ErrEagain) && !errors.Is(err, astiav.ErrEof) {
					return fmt.Errorf("ffmpeg: flushing resampled audio: %w", err)
				}
			} else if ass.encoder.frame.NbSamples() > 0 {
				if _, err := ass.encoder.fifo.Write(ass.encoder.frame); err != nil {
					ass.encoder.frame.Unref()
					return fmt.Errorf("ffmpeg: writing flushed resampled audio to FIFO: %w", err)
				}

				ass.encoder.frame.Unref()
				ass.encoder.frame.SetChannelLayout(ass.encoder.codecContext.ChannelLayout())
				ass.encoder.frame.SetSampleFormat(ass.encoder.codecContext.SampleFormat())
				ass.encoder.frame.SetSampleRate(ass.encoder.codecContext.SampleRate())
			}
		}
		// Drain any complete frames remaining in the FIFO.
		if err := ass.drainFifo(outputFmt, progressCh, totalDuration); err != nil {
			return err
		}
		// Encode any remaining sub-frame tail padded with silence so no audio is
		// truncated at end-of-stream.
		if remaining := ass.encoder.fifo.Size(); remaining > 0 {
			frameSize := ass.encoder.codecContext.FrameSize()
			if err := ass.encoder.fifoFrame.MakeWritable(); err != nil {
				return fmt.Errorf("ffmpeg: making tail FIFO frame writable: %w", err)
			}

			ass.encoder.fifoFrame.SetNbSamples(frameSize)

			if err := ass.encoder.fifoFrame.SamplesFillSilence(); err != nil {
				return fmt.Errorf("ffmpeg: zeroing tail audio frame: %w", err)
			}
			// Read only the remaining samples so they land at the start of the
			// silence-padded buffer.
			ass.encoder.fifoFrame.SetNbSamples(remaining)

			if _, err := ass.encoder.fifo.Read(ass.encoder.fifoFrame); err != nil {
				return fmt.Errorf("ffmpeg: reading tail samples from audio FIFO: %w", err)
			}
			// Restore full frame size so the encoder receives a complete frame.
			ass.encoder.fifoFrame.SetNbSamples(frameSize)
			ass.encoder.fifoFrame.SetPts(ass.encoder.nextPts)

			if err := ass.encoder.codecContext.SendFrame(ass.encoder.fifoFrame); err != nil {
				return fmt.Errorf("ffmpeg: sending tail audio frame to encoder: %w", err)
			}

			if err := ass.receiveAndWritePackets(ass.encoder.codecContext, ass.encoder.packet, outputFmt, progressCh, totalDuration); err != nil {
				return err
			}
		}
	}

	// Flush the encoder.
	if err := ass.encoder.codecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing audio encoder: %w", err)
	}

	return ass.receiveAndWritePackets(ass.encoder.codecContext, ass.encoder.packet, outputFmt, progressCh, totalDuration)
}
