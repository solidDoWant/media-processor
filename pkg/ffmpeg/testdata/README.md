# Test Data

## video_black_bars.mp4

Derived from `pkg/ffprobe/testdata/video.mp4` (first ~5 seconds of Big Buck Bunny at 320x180) by padding it with 20px black bars on the top and bottom, producing a 320x220 file.

Generation command:

```bash
ffmpeg -i pkg/ffprobe/testdata/video.mp4 -vf "pad=iw:ih+40:0:20:black" pkg/ffmpeg/testdata/video_black_bars.mp4
```

- **Source**: [Big Buck Bunny](https://peach.blender.org/) by Blender Foundation
- **License**: [Creative Commons Attribution 3.0 (CC BY 3.0)](https://creativecommons.org/licenses/by/3.0/)
- **Copyright**: © 2008, Blender Foundation
- **Attribution**: "Big Buck Bunny" by Blender Foundation (https://www.blender.org)

## video_mpeg4.avi

A 2 s 320x180 mpeg4-ASP (Simple Profile) clip in an AVI container, transcoded from the first 2 seconds of `pkg/ffprobe/testdata/video.mp4`. mpeg4-ASP has no Intel hardware decoder, so this fixture deterministically forces the software-decode + hardware-encode pipeline: decoded `yuv420p` frames are scaled to `NV12` on the CPU and uploaded to a GPU surface before encoding. Used by the QSV/VAAPI software-decode regression tests (`TestTranscode_SoftwareDecodeToQSV`, `TestTranscode_SoftwareDecodeToVAAPI`), which transcode it to H.265 with no crop. Before the fix the software scaler's destination frame and the GPU upload surface shared a single field, so swscale wrote into a hardware surface and failed with "scaling video frame: Invalid argument" (swscale's "bad dst image pointers").

Generation command:

```bash
ffmpeg -y -i pkg/ffprobe/testdata/video.mp4 -t 2 -an -c:v mpeg4 -q:v 4 pkg/ffmpeg/testdata/video_mpeg4.avi
```

- **Source**: [Big Buck Bunny](https://peach.blender.org/) by Blender Foundation
- **License**: [Creative Commons Attribution 3.0 (CC BY 3.0)](https://creativecommons.org/licenses/by/3.0/)
- **Copyright**: © 2008, Blender Foundation
- **Attribution**: "Big Buck Bunny" by Blender Foundation (https://www.blender.org)

## video_short_bars.mp4

A synthetic 12-frame (0.5 s at 24 fps) H.264 clip used to verify crop detection on videos shorter than the `sampleInterval` threshold. The video is 320x220: solid-blue 320x180 content padded with 20 px black bars on the top and bottom, encoded with B-frames (bframes=3) and a single keyframe.

Generation command:

```bash
ffmpeg -f lavfi -i "color=c=blue:size=320x180:rate=24:duration=0.5" -vf "pad=iw:ih+40:0:20:black" -c:v libx264 -y pkg/ffmpeg/testdata/video_short_bars.mp4
```

Expected `cropdetect` output: `crop=320:176:0:22` (round=16 reduces 180 → 176; y=22 centers the window).

## cover.jpg

A 100x100 solid-green JPEG used as a cover-art payload in transcode tests that need to verify the bytes that end up in the output MKV's attachment stream.

Generation command:

```bash
ffmpeg -y -f lavfi -i color=c=green:s=100x100:d=1 -frames:v 1 -q:v 5 \
       pkg/ffmpeg/testdata/cover.jpg
```

## video_with_movtext_subtitle.mp4

A synthetic mp4 with a 0.5 s 160x120 H.264 video, a 0.5 s AAC audio track, and a single mov_text subtitle ("Hello world"). The matroska muxer rejects mov_text outright (`Subtitle codec mov_text (94213) is not supported.`), so this fixture is used to regression-test the conversion of mov_text to a matroska-compatible subtitle codec on the way through the transcoder.

Generation command:

```bash
ffmpeg -y -f lavfi -i color=c=blue:s=160x120:rate=24:duration=0.5 \
       -f lavfi -i sine=frequency=440:duration=0.5 \
       -c:v libx264 -preset ultrafast -crf 35 -pix_fmt yuv420p \
       -c:a aac -b:a 64k main.mp4
printf '1\n00:00:00,000 --> 00:00:00,500\nHello world\n\n' > sub.srt
ffmpeg -y -i main.mp4 -i sub.srt -map 0 -map 1 -c copy -c:s mov_text \
       pkg/ffmpeg/testdata/video_with_movtext_subtitle.mp4
```

## video_with_subrip_subtitle.mkv

A synthetic MKV with a 0.5 s 160x120 H.264 video, a 0.5 s AAC audio track, and a single SubRip subtitle ("Hello world"). Used to verify that subtitle codecs the matroska muxer already supports natively (here, `subrip` → `S_TEXT/UTF8`) are passed through by copy and are *not* re-routed into the mov_text → ASS transcode pipeline.

Generation command:

```bash
ffmpeg -y -f lavfi -i color=c=blue:s=160x120:rate=24:duration=0.5 \
       -f lavfi -i sine=frequency=440:duration=0.5 \
       -c:v libx264 -preset ultrafast -crf 35 -pix_fmt yuv420p \
       -c:a aac -b:a 64k main.mp4
printf '1\n00:00:00,000 --> 00:00:00,500\nHello world\n\n' > sub.srt
ffmpeg -y -i main.mp4 -i sub.srt -map 0 -map 1 -c copy -c:s srt \
       pkg/ffmpeg/testdata/video_with_subrip_subtitle.mkv
```

## video_adts_aac.ts

A synthetic MPEG-TS with a 0.5 s 160x120 H.264 video and a 0.5 s AAC audio track. Unlike the mp4 and matroska fixtures, the AAC here is ADTS-framed, so the track carries no `AudioSpecificConfig` extradata — the shape every off-air TS recording has. matroska needs that config to record the track's sample rate and derives it by auto-inserting the `aac_adtstoasc` bitstream filter, but only when the first packet submitted for the stream starts with an ADTS syncword. Used to regression-test that a leading packet which is *not* a well-formed ADTS frame (what the demuxer's AAC parser emits when a capture began mid-frame) is dropped instead of suppressing that filter and leaving the muxer to reject packets with "Invalid argument".

Generation command:

```bash
ffmpeg -y -f lavfi -i color=c=blue:s=160x120:rate=24:duration=0.5 \
       -f lavfi -i sine=frequency=440:duration=0.5 \
       -c:v libx264 -preset ultrafast -crf 35 -pix_fmt yuv420p \
       -c:a aac -b:a 64k \
       pkg/ffmpeg/testdata/video_adts_aac.ts
```

The fixture's packets are all intact frames; the malformed leading packet is rebuilt in the test by slicing the header off a real one, so the regression is precisely controlled rather than dependent on a hand-built transport stream.

## video_with_data_stream.mp4

A synthetic mp4 with a 0.5 s 160x120 H.264 video, a 0.5 s AAC audio track, and a QuickTime timecode (`tmcd`) track that ffmpeg surfaces as a data stream (`codec_type=data`). matroska's track muxer rejects anything other than audio/video/subtitle ("Only audio, video, and subtitles are supported for Matroska"), so this fixture regression-tests that the transcoder drops matroska-incompatible streams instead of letting them reach WriteHeader. The same data-stream class shows up in many real-world mp4 sources as `bin_data` from QuickTime metadata, chapter, or index tracks.

Generation command:

```bash
ffmpeg -y -f lavfi -i color=c=blue:s=160x120:rate=24:duration=0.5 \
       -f lavfi -i sine=frequency=440:duration=0.5 \
       -c:v libx264 -preset ultrafast -crf 35 -pix_fmt yuv420p \
       -c:a aac -b:a 64k \
       -timecode 01:00:00:00 \
       pkg/ffmpeg/testdata/video_with_data_stream.mp4
```

## video_with_image_stream.mp4

A synthetic mp4 carrying a 0.5 s 160x120 H.264 main video plus a bare mjpeg "video" stream (a 200x300 cover image, *no* `disposition:attached_pic`). This mirrors how iTunes/Plex-derived mp4 files commonly carry a preview thumbnail as a second video stream rather than as `attached_pic`. Without specifically dropping these the transcoder re-encodes the still as a single-frame HEVC stream, so the output `.mkv` ends up with two video streams: the real movie plus a useless thumbnail.

Generation command:

```bash
ffmpeg -y -f lavfi -i color=c=blue:s=160x120:rate=24:duration=0.5 \
       -c:v libx264 -preset ultrafast -crf 35 -pix_fmt yuv420p main.mp4
ffmpeg -y -f lavfi -i color=c=red:s=200x300:d=1 -frames:v 1 -q:v 8 thumb.jpg
ffmpeg -y -i main.mp4 -i thumb.jpg \
       -map 0:v -map 1 -c copy \
       pkg/ffmpeg/testdata/video_with_image_stream.mp4
```

## video_with_attached_pic.mp4

A synthetic mp4 with two video streams: a 0.25 s 160x120 H.264 main video and a 200x300 mjpeg cover-art image carrying `disposition:attached_pic`. Used to regression-test the embedded-cover-art exclusion in `Transcoder` (a missing exclusion fed the still image into the HEVC encoder; on QSV this returned "Function not implemented").

Generation command:

```bash
ffmpeg -y -f lavfi -i color=c=blue:s=160x120:rate=24:duration=0.25 \
       -c:v libx264 -preset ultrafast -crf 35 -pix_fmt yuv420p main.mp4
ffmpeg -y -f lavfi -i color=c=red:s=200x300:d=1 -frames:v 1 -q:v 8 poster.jpg
ffmpeg -y -i main.mp4 -i poster.jpg \
       -map 0:v -map 1 -c copy -disposition:v:1 attached_pic \
       pkg/ffmpeg/testdata/video_with_attached_pic.mp4
```

## video_vfr_hevc.mkv

A synthetic 5 s 640x360 HEVC clip whose container PTS values have been rewritten to inject the variable-frame-rate pattern that triggers `hevc_qsv` on Intel Arc to emit non-monotonic DTS. The bitstream itself is normal CFR HEVC at 24000/1001 fps with `bf=3` B-frames; only the matroska packet timestamps have been perturbed. Used to regression-test that the transcoder's `receiveAndWritePackets` clamp keeps the muxer from rejecting these packets when re-encoding through QSV.

The fixture is unlikely to need regeneration — the failure mode it captures is stable. If you do need to rebuild it (e.g. to dial up or down the irregularity), the recipe is two steps: encode a clean CFR base, then rewrite packet PTS in place.

Step 1, the clean CFR base:

```bash
ffmpeg -y -f lavfi -i "testsrc=duration=5:size=640x360:rate=24000/1001" \
       -c:v libx265 -preset ultrafast -bf 3 \
       -x265-params "log-level=error:bframes=3:b-pyramid=1" \
       -pix_fmt yuv420p \
       cfr.mkv
```

Step 2, the PTS injection. Read every video packet from `cfr.mkv` in coding order. Sort indices by PTS to derive display order. Build a per-display-position shift table that mimics the production source's gap pattern — starting at display position 50, apply a persistent +230 ms baseline shift; then at successive display positions add cumulative ramps so the PTS gaps in display order become 126 / 105 / 84 / 42 / 42 / 42 / 42 / 42 / 230 / 42 / 34 / 35 / 37 ms before settling back to ~42 ms. Apply each display position's shift to the PTS of the corresponding packet in coding order. Leave DTS untouched. Write the modified packets to a new matroska file with `c copy`.

DTS stays valid because every shift is non-negative — DTS monotonicity from the original encode is preserved, and `DTS ≤ PTS` cannot be violated by raising PTS only. The HEVC bitstream is unchanged, so the resulting file is a well-formed HEVC matroska that nonetheless trips the libmfx/oneVPL DTS computation when re-encoded through QSV.

A reference Go implementation using `go-astiav` lived at `cmd/gen-vfr-fixture/` during the original investigation; see git history if it would help as a starting point.

## video_all_black.mp4

A synthetic 12-frame (0.5 s at 24 fps) lossless H.264 clip used to verify that `DetectCrop` returns an error when no visible content is present. The video is 320x180 solid black (CRF 0, so decoded pixels are exactly zero).

When `cropdetect` processes an all-black frame it emits inverted sentinel values (w &lt; 0, h &lt; 0). `parseCropMetadata` rejects those values, `haveCrop` stays false, and `DetectCrop` returns "no crop metadata produced".

Generation command:

```
ffmpeg -f lavfi -i "color=black:size=320x180:rate=24:duration=0.5" -c:v libx264 -crf 0 -y pkg/ffmpeg/testdata/video_all_black.mp4
```

## video_shifted_ts.ts

A 2 s 160x120 MPEG-TS clip whose container start time is 10001.4 s, with its audio stream starting 0.3 s after its video stream. It stands in for an off-air recording, which routinely begins at an arbitrary PTS rather than near zero. Used to regression-test that output timestamps are rebased onto a zero-based timeline: without the rebase the output inherits the source's start time, so a 2 s clip reports a duration of 10003.4 s, players show hours of nothing before the content, progress reporting is pinned at 100% from the first packet, and the transcode reuse check never matches. The deliberate 0.3 s gap between the streams is what makes the A/V sync assertion falsifiable — a per-stream offset would collapse both streams onto zero and pass a fixture whose streams start together.

Generation command:

```bash
ffmpeg -y -i pkg/ffprobe/testdata/video.mp4 -itsoffset 0.3 -i pkg/ffprobe/testdata/video.mp4 \
       -map 0:v:0 -map 1:a:0 -t 2 -vf scale=160:120 \
       -c:v libx264 -preset veryslow -crf 32 -c:a aac -b:a 32k -ac 1 \
       -output_ts_offset 10000 -f mpegts \
       pkg/ffmpeg/testdata/video_shifted_ts.ts
```

The second input is the same file read with a 0.3 s input offset, which is what puts the audio stream behind the video one. `-output_ts_offset` then shifts the whole muxed timeline forward.

- **Source**: [Big Buck Bunny](https://peach.blender.org/) by Blender Foundation
- **License**: [Creative Commons Attribution 3.0 (CC BY 3.0)](https://creativecommons.org/licenses/by/3.0/)
- **Copyright**: © 2008, Blender Foundation
- **Attribution**: "Big Buck Bunny" by Blender Foundation (https://www.blender.org)
