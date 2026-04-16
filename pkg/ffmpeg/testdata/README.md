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
