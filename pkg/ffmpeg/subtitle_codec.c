#include "subtitle_codec.h"

#include <libavcodec/avcodec.h>
#include <libavcodec/codec_desc.h>
#include <libavcodec/defs.h>
#include <libavformat/avformat.h>
#include <libavutil/error.h>
#include <libavutil/mem.h>
#include <libavutil/rational.h>
#include <stdint.h>
#include <string.h>

AVCodecContext* mpsub_decoder_open(int codec_id, const uint8_t* extradata,
                                   int extradata_size,
                                   int pkt_timebase_num, int pkt_timebase_den,
                                   int* err) {
    const AVCodec* codec = avcodec_find_decoder((enum AVCodecID)codec_id);
    if (!codec) {
        *err = AVERROR_DECODER_NOT_FOUND;
        return NULL;
    }

    AVCodecContext* ctx = avcodec_alloc_context3(codec);
    if (!ctx) {
        *err = AVERROR(ENOMEM);
        return NULL;
    }

    // Subtitle codecs need a non-zero time base or avcodec_open2 may reject
    // them. 1/1000 (millisecond) matches what most demuxers report and what
    // the ASS encoder uses internally for timestamp rescales.
    ctx->time_base.num = 1;
    ctx->time_base.den = 1000;

    // pkt_timebase is the timebase the libavcodec subtitle decode wrapper
    // expects AVPacket.pts/duration to be expressed in. Without this the
    // wrapper silently skips its sub->pts / sub->end_display_time rescale
    // (the `if (avctx->pkt_timebase.num)` guard in avcodec_decode_subtitle2),
    // which happens to be benign for the matroska→ASS path we use today
    // — the ASS encoder doesn't read sub->pts and matroska Block timing
    // comes from the output packet's pts/duration via av_packet_copy_props
    // — but it would silently break any future converter target whose
    // encoder consumes sub->pts (e.g. SubRip via libavcodec).
    if (pkt_timebase_num > 0 && pkt_timebase_den > 0) {
        ctx->pkt_timebase.num = pkt_timebase_num;
        ctx->pkt_timebase.den = pkt_timebase_den;
    }

    if (extradata_size > 0) {
        ctx->extradata = av_mallocz((size_t)extradata_size + AV_INPUT_BUFFER_PADDING_SIZE);
        if (!ctx->extradata) {
            *err = AVERROR(ENOMEM);
            avcodec_free_context(&ctx);
            return NULL;
        }

        memcpy(ctx->extradata, extradata, (size_t)extradata_size);
        ctx->extradata_size = extradata_size;
    }

    int rc = avcodec_open2(ctx, codec, NULL);
    if (rc < 0) {
        *err = rc;
        avcodec_free_context(&ctx);
        return NULL;
    }

    *err = 0;
    return ctx;
}

AVCodecContext* mpsub_encoder_open(int codec_id, AVCodecContext* decoder,
                                   int* err) {
    const AVCodec* codec = avcodec_find_encoder((enum AVCodecID)codec_id);
    if (!codec) {
        *err = AVERROR_ENCODER_NOT_FOUND;
        return NULL;
    }

    AVCodecContext* ctx = avcodec_alloc_context3(codec);
    if (!ctx) {
        *err = AVERROR(ENOMEM);
        return NULL;
    }

    ctx->time_base.num = 1;
    ctx->time_base.den = 1000;

    if (decoder && decoder->subtitle_header_size > 0) {
        ctx->subtitle_header = av_mallocz((size_t)decoder->subtitle_header_size + 1);
        if (!ctx->subtitle_header) {
            *err = AVERROR(ENOMEM);
            avcodec_free_context(&ctx);
            return NULL;
        }

        memcpy(ctx->subtitle_header, decoder->subtitle_header,
               (size_t)decoder->subtitle_header_size);
        ctx->subtitle_header_size = decoder->subtitle_header_size;
    }

    int rc = avcodec_open2(ctx, codec, NULL);
    if (rc < 0) {
        *err = rc;
        avcodec_free_context(&ctx);
        return NULL;
    }

    *err = 0;
    return ctx;
}

void mpsub_codec_close(AVCodecContext** ctx) {
    avcodec_free_context(ctx);
}

const uint8_t* mpsub_extradata(AVCodecContext* ctx, int* size) {
    *size = ctx->extradata_size;
    return ctx->extradata;
}

int mpsub_convert(AVCodecContext* dec, AVCodecContext* enc,
                  const uint8_t* in_data, int in_size,
                  int64_t pts, int64_t duration,
                  uint8_t* out_buf, int out_buf_size) {
    AVPacket* in_pkt = av_packet_alloc();
    if (!in_pkt) {
        return AVERROR(ENOMEM);
    }

    // av_packet_from_data takes ownership of the buffer it is given, so a
    // freshly-malloc'd copy of the Go-supplied bytes is what we hand it.
    uint8_t* buf = av_malloc((size_t)in_size + AV_INPUT_BUFFER_PADDING_SIZE);
    if (!buf) {
        av_packet_free(&in_pkt);
        return AVERROR(ENOMEM);
    }

    memcpy(buf, in_data, (size_t)in_size);
    memset(buf + in_size, 0, AV_INPUT_BUFFER_PADDING_SIZE);

    int rc = av_packet_from_data(in_pkt, buf, in_size);
    if (rc < 0) {
        av_free(buf);
        av_packet_free(&in_pkt);

        return rc;
    }

    in_pkt->pts = pts;
    in_pkt->duration = duration;

    AVSubtitle sub;
    memset(&sub, 0, sizeof(sub));

    int got_sub = 0;
    rc = avcodec_decode_subtitle2(dec, &sub, &got_sub, in_pkt);
    av_packet_free(&in_pkt);

    if (rc < 0) {
        avsubtitle_free(&sub);
        return rc;
    }

    if (!got_sub) {
        avsubtitle_free(&sub);
        return 0;
    }

    int written = avcodec_encode_subtitle(enc, out_buf, out_buf_size, &sub);
    avsubtitle_free(&sub);

    return written;
}

void mpsub_strerror(int code, char* buf, int buf_size) {
    av_strerror(code, buf, (size_t)buf_size);
}

int mpsub_matroska_supports(int codec_id) {
    const AVOutputFormat* ofmt = av_guess_format("matroska", NULL, NULL);
    if (!ofmt) {
        return -1;
    }

    return avformat_query_codec(ofmt, (enum AVCodecID)codec_id, FF_COMPLIANCE_NORMAL);
}

int mpsub_is_text_subtitle(int codec_id) {
    const AVCodecDescriptor* desc = avcodec_descriptor_get((enum AVCodecID)codec_id);
    if (!desc) {
        return 0;
    }

    return (desc->props & AV_CODEC_PROP_TEXT_SUB) ? 1 : 0;
}

int mpsub_codec_has_image_mime_type(int codec_id) {
    const AVCodecDescriptor* desc = avcodec_descriptor_get((enum AVCodecID)codec_id);
    if (!desc || !desc->mime_types) {
        return 0;
    }

    for (int i = 0; desc->mime_types[i]; i++) {
        if (strncmp(desc->mime_types[i], "image/", 6) == 0) {
            return 1;
        }
    }

    return 0;
}
