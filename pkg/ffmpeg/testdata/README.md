# Test Data

## video_black_bars.mp4

This file is derived from `pkg/ffprobe/testdata/video.mp4` (the Big Buck Bunny
excerpt, 320×180) by adding 20-pixel horizontal black bars at the top and
bottom, producing a 320×220 file.

**Generation command:**

```sh
ffmpeg -i ../../../ffprobe/testdata/video.mp4 \
  -vf "pad=iw:ih+40:0:20:black" \
  testdata/video_black_bars.mp4
```

It is used as a test fixture for `DetectCrop` to verify that cropdetect
correctly identifies and returns the non-black content region.

### Attribution

- **Source**: [Big Buck Bunny](https://peach.blender.org/) by Blender Foundation
- **License**: [Creative Commons Attribution 3.0 (CC BY 3.0)](https://creativecommons.org/licenses/by/3.0/)
- **Copyright**: © 2008, Blender Foundation
- **Attribution**: "Big Buck Bunny" by Blender Foundation (https://www.blender.org)

This file is a derivative of the original and is used solely as a test fixture
for the `pkg/ffmpeg` package.
