# Test Data

## video_black_bars.mp4

Derived from `pkg/ffprobe/testdata/video.mp4` (first ~5 seconds of Big Buck Bunny at 320x180) by padding it with 20px black bars on the top and bottom, producing a 320x220 file.

Generation command:

```
ffmpeg -i pkg/ffprobe/testdata/video.mp4 -vf "pad=iw:ih+40:0:20:black" pkg/ffmpeg/testdata/video_black_bars.mp4
```

- **Source**: [Big Buck Bunny](https://peach.blender.org/) by Blender Foundation
- **License**: [Creative Commons Attribution 3.0 (CC BY 3.0)](https://creativecommons.org/licenses/by/3.0/)
- **Copyright**: © 2008, Blender Foundation
- **Attribution**: "Big Buck Bunny" by Blender Foundation (https://www.blender.org)

## video_short_bars.mp4

A synthetic 12-frame (0.5 s at 24 fps) H.264 clip used to verify crop detection on videos shorter than the `sampleInterval` threshold. The video is 320x220: solid-blue 320x180 content padded with 20 px black bars on the top and bottom, encoded with B-frames (bframes=3) and a single keyframe.

Generation command:

```
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

## video_all_black.mp4

A synthetic 12-frame (0.5 s at 24 fps) lossless H.264 clip used to verify that `DetectCrop` returns an error when no visible content is present. The video is 320x180 solid black (CRF 0, so decoded pixels are exactly zero).

When `cropdetect` processes an all-black frame it emits inverted sentinel values (w &lt; 0, h &lt; 0). `parseCropMetadata` rejects those values, `haveCrop` stays false, and `DetectCrop` returns "no crop metadata produced".

Generation command:

```
ffmpeg -f lavfi -i "color=black:size=320x180:rate=24:duration=0.5" -c:v libx264 -crf 0 -y pkg/ffmpeg/testdata/video_all_black.mp4
```
