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

## video_no_bars.mp4

A synthetic 1-second, 320x160 solid-green video (libx264, lossless). Used to verify that `DetectCrop` returns full input dimensions when the video has no black bars. The height is chosen to be a multiple of 16 so that cropdetect's default `round=16` parameter does not reduce the reported height.

Generation command:

```
ffmpeg -f lavfi -i "color=c=0x3a7a3a:s=320x160:d=1:r=24" -c:v libx264 -crf 0 pkg/ffmpeg/testdata/video_no_bars.mp4
```

This file is entirely synthetic and is not derived from any copyrighted work.
