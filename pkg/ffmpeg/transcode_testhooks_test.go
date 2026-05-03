package ffmpeg

// This file is only compiled into the ffmpeg package's test binary. The
// WithStartHook builder method is exported so external test packages
// (package ffmpeg_test) can use it, but it does not appear in production
// builds at all.

// WithStartHook sets a function that is called once the transcoder has
// finished all setup and is about to enter the main packet read loop.
// Used by tests to trigger context cancellation at a deterministic point.
func (b *TranscodeBuilder) WithStartHook(fn func()) *TranscodeBuilder {
	b.startHook = fn
	return b
}
