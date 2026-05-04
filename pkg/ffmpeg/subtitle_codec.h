// Subtitle codec helpers used by stream_subtitle.go to transcode subtitle
// streams across containers (notably mov_text → ASS for mp4 → matroska
// output). All functions return libav AVERROR codes on failure.

#ifndef MEDIA_PROCESSOR_SUBTITLE_CODEC_H
#define MEDIA_PROCESSOR_SUBTITLE_CODEC_H

#include <libavcodec/avcodec.h>
#include <stdint.h>

// mpsub_decoder_open allocates and opens a subtitle decoder for the given
// codec ID. The input stream's extradata (e.g. mp4 TextSampleEntry for
// mov_text) is copied onto the context so the decoder can derive the ASS
// [V4+ Styles] header that downstream encoders embed in their own extradata.
// On failure, *err is set to a negative AVERROR and NULL is returned; the
// caller frees with mpsub_codec_close.
AVCodecContext* mpsub_decoder_open(int codec_id, const uint8_t* extradata,
                                   int extradata_size, int* err);

// mpsub_encoder_open allocates and opens a subtitle encoder for the given
// codec ID. The decoder's subtitle_header (the [V4+ Styles] section the
// decoder produced from the input stream's defaults) is copied into the
// encoder before opening so the encoder embeds matching defaults in its own
// extradata — this is what gets attached to the output stream so matroska
// players know how to render the dialogue.
AVCodecContext* mpsub_encoder_open(int codec_id, AVCodecContext* decoder,
                                   int* err);

// mpsub_codec_close frees a codec context and zeroes the caller's pointer.
void mpsub_codec_close(AVCodecContext** ctx);

// mpsub_extradata returns the codec context's extradata buffer and size,
// without copying. The returned pointer is owned by the codec context and
// must not outlive it.
const uint8_t* mpsub_extradata(AVCodecContext* ctx, int* size);

// mpsub_convert decodes one input subtitle packet and re-encodes it into the
// caller-provided buffer in a single call. Decoupling decode and encode in
// Go would buy nothing here: the AVSubtitle intermediate is a C-owned struct
// and neither the decoder nor encoder buffer state across calls (subtitles
// are stateless per packet, unlike audio/video).
//
// Return values:
//   >0  number of encoded bytes written to out_buf
//    0  the decoder accepted the packet but produced no subtitle (e.g.
//       mov_text "clear subtitle" markers) — caller should drop the event
//   <0  AVERROR on either decode or encode
int mpsub_convert(AVCodecContext* dec, AVCodecContext* enc,
                  const uint8_t* in_data, int in_size,
                  int64_t pts, int64_t duration,
                  uint8_t* out_buf, int out_buf_size);

// mpsub_strerror writes a human-readable description of an AVERROR into buf.
void mpsub_strerror(int code, char* buf, int buf_size);

// mpsub_matroska_supports asks libavformat whether the matroska muxer can
// write the given codec ID directly. This is the same lookup the muxer
// performs internally when choosing a codec_tag for an output stream, so it
// never drifts from what would actually succeed at WriteHeader. Returns 1
// when supported, 0 when unsupported, and -1 if the matroska muxer is not
// registered (which would mean libavformat was built without it).
int mpsub_matroska_supports(int codec_id);

// mpsub_is_text_subtitle reports whether the codec descriptor flags the
// codec as a text-based subtitle (vs bitmap or non-subtitle). Used to gate
// transcode-to-ASS: bitmap subtitles like PGS, VobSub, and DVB cannot be
// converted to ASS at all (ASS is text), so they must be left alone even
// when the muxer doesn't accept them.
int mpsub_is_text_subtitle(int codec_id);

#endif // MEDIA_PROCESSOR_SUBTITLE_CODEC_H
