package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
)

func newWorkflowActivities(t *testing.T) *Activities {
	t.Helper()

	a, err := NewActivities(
		MediaWorkflowConfig{DetectCropTimeout: 30 * time.Minute, TranscodeTimeout: 4 * time.Hour},
		&stubLibraryClient{},
		&stubLibraryClient{},
		&webhook.Client{},
	)
	require.NoError(t, err)

	return a
}

func TestMediaWorkflow_ValidPath_RunsAllActivitiesInOrder(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	newWorkflowActivities(t).Register(env)

	probeOut := ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	cropOut := DetectCropOutput{}
	transOut := TranscodeOutput{DestCodec: "hevc", DestContainer: "mkv", DestFilePath: "/out/file.mkv"}

	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(cropOut, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(transOut, nil).Once()
	env.OnActivity(NotifyActivityName, mock.Anything, mock.Anything, mock.Anything).Return(NotifyOutput{}, nil).Once()
	env.OnActivity(CleanupActivityName, mock.Anything, mock.Anything, expectTranscode(transOut), mock.Anything).Return(nil).Once()

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestMediaWorkflow_SkipCropDetection_SkipsDetectCropAndTranscodesWithoutCrop(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	newWorkflowActivities(t).Register(env)

	probeOut := ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	transOut := TranscodeOutput{DestCodec: "hevc", DestContainer: "mkv", DestFilePath: "/out/file.mkv"}

	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	// DetectCrop must NOT be invoked: no .Return is registered for it, so the
	// mock fails the test if it is called. Transcode must receive a zero-value
	// DetectCropOutput (nil crop) so the full frame is transcoded.
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, expectCrop(DetectCropOutput{})).
		Return(transOut, nil).Once()
	env.OnActivity(NotifyActivityName, mock.Anything, mock.Anything, mock.Anything).Return(NotifyOutput{}, nil).Once()
	env.OnActivity(CleanupActivityName, mock.Anything, mock.Anything, expectTranscode(transOut), mock.Anything).Return(nil).Once()

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{
		FilePath:          "/in/file.mp4",
		MediaType:         medialib.MovieType,
		OutputPath:        "/out",
		SkipCropDetection: true,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestMediaWorkflow_InvalidPath_SkipsTranscodeAndCallsCleanup(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	newWorkflowActivities(t).Register(env)

	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).
		Return(ProbeOutput{IsValidMedia: false}, nil).Once()
	// Invalid-media path skips Transcode; Cleanup must receive a zero-value
	// TranscodeOutput so output-side pruning is suppressed.
	env.OnActivity(CleanupActivityName, mock.Anything, mock.Anything, expectTranscode(TranscodeOutput{}), mock.Anything).Return(nil).Once()

	// DetectCrop, Transcode, and Notify must NOT be invoked. The mock fails
	// the test if any unexpected call arrives because no .Return was
	// registered for them.

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.txt", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

// TestMediaWorkflow_NotInLibrarySkipsImportAndCleansUp verifies that when
// Notify reports the media item is no longer in the library (ImportSkipped),
// the workflow completes successfully without firing the failure webhook and
// forwards the skip signal to Cleanup so the orphaned output is removed.
func TestMediaWorkflow_NotInLibrarySkipsImportAndCleansUp(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	newWorkflowActivities(t).Register(env)

	probeOut := ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	transOut := TranscodeOutput{DestCodec: "hevc", DestContainer: "mkv", DestFilePath: "/out/file.mkv"}

	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(DetectCropOutput{}, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(transOut, nil).Once()
	env.OnActivity(NotifyActivityName, mock.Anything, mock.Anything, mock.Anything).Return(NotifyOutput{ImportSkipped: true}, nil).Once()
	// Cleanup must receive the skip signal so it removes the orphaned output.
	env.OnActivity(CleanupActivityName, mock.Anything, mock.Anything, expectTranscode(transOut), expectNotify(NotifyOutput{ImportSkipped: true})).
		Return(nil).Once()

	// NotifyFailure must NOT be invoked: a skipped import is not a failure. No
	// .Return is registered for it, so the mock fails the test if it is called.

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "a skipped import is a benign success, not a workflow failure")
	env.AssertExpectations(t)
}

func TestMediaWorkflow_TranscodeFailureFiresFailureWebhook(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	newWorkflowActivities(t).Register(env)

	probeOut := ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(DetectCropOutput{}, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(TranscodeOutput{}, errors.New("ffmpeg blew up")).Once()

	var seenStep, seenMessage string

	env.OnActivity(NotifyFailureActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ MediaInput, step, message string) error {
			seenStep, seenMessage = step, message
			return nil
		}).Once()

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError(), "workflow should propagate the activity failure")
	assert.Equal(t, TranscodeActivityName, seenStep)
	assert.Contains(t, seenMessage, "ffmpeg blew up")
	env.AssertExpectations(t)
}

func TestMediaWorkflow_ProbeFailureFiresFailureWebhook(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	newWorkflowActivities(t).Register(env)

	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).
		Return(ProbeOutput{}, errors.New("probe failed")).Once()

	var seenStep string

	env.OnActivity(NotifyFailureActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ MediaInput, step, _ string) error {
			seenStep = step
			return nil
		}).Once()

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	assert.Equal(t, ProbeActivityName, seenStep)
}

// TestMediaWorkflow_NotifyAndCleanupRetry verifies that the notify and cleanup
// activities each retry up to 3 times when their first attempts fail
// transiently.
func TestMediaWorkflow_NotifyAndCleanupRetry(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	newWorkflowActivities(t).Register(env)

	probeOut := ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(DetectCropOutput{}, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(TranscodeOutput{DestFilePath: "/out/file.mkv"}, nil).Once()

	notifyAttempts := 0
	cleanupAttempts := 0

	env.OnActivity(NotifyActivityName, mock.Anything, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ MediaInput, _ TranscodeOutput) (NotifyOutput, error) {
			notifyAttempts++
			if notifyAttempts < 3 {
				return NotifyOutput{}, errors.New("transient notify failure")
			}

			return NotifyOutput{}, nil
		})

	env.OnActivity(CleanupActivityName, mock.Anything, mock.Anything, expectTranscode(TranscodeOutput{DestFilePath: "/out/file.mkv"}), mock.Anything).
		Return(func(_ context.Context, _ MediaInput, _ TranscodeOutput, _ NotifyOutput) error {
			cleanupAttempts++
			if cleanupAttempts < 3 {
				return errors.New("transient cleanup failure")
			}

			return nil
		})

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, 3, notifyAttempts, "notify should retry up to 3 times")
	assert.Equal(t, 3, cleanupAttempts, "cleanup should retry up to 3 times")
}

// TestMediaWorkflow_NonRetryableInputErrorOnNotifyDoesNotRetry verifies that
// the workflow does not burn the notify retry budget on a pure-data error.
// The non-retryable ApplicationError stops Temporal at the first attempt.
func TestMediaWorkflow_NonRetryableInputErrorOnNotifyDoesNotRetry(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	newWorkflowActivities(t).Register(env)

	probeOut := ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(DetectCropOutput{}, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(TranscodeOutput{DestFilePath: "/out/file.mkv"}, nil).Once()

	notifyAttempts := 0

	env.OnActivity(NotifyActivityName, mock.Anything, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ MediaInput, _ TranscodeOutput) (NotifyOutput, error) {
			notifyAttempts++
			return NotifyOutput{}, temporal.NewNonRetryableApplicationError("unknown media type", errTypeNonRetryable, nil)
		})

	env.OnActivity(NotifyFailureActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	assert.Equal(t, 1, notifyAttempts, "non-retryable application error should stop Temporal at the first attempt")
}

func TestMediaWorkflow_NonRetryableActivitiesFailOnFirstError(t *testing.T) {
	tests := []struct {
		name        string
		failingStep string
		setupMocks  func(env *testsuite.TestWorkflowEnvironment, attempts *int)
	}{
		{
			name:        "probe is invoked exactly once on failure (no retries)",
			failingStep: ProbeActivityName,
			setupMocks: func(env *testsuite.TestWorkflowEnvironment, attempts *int) {
				env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).
					Return(func(_ context.Context, _ MediaInput) (ProbeOutput, error) {
						*attempts++
						return ProbeOutput{}, errors.New("probe failed")
					})
			},
		},
		{
			name:        "detectcrop is invoked exactly once on failure (no retries)",
			failingStep: DetectCropActivityName,
			setupMocks: func(env *testsuite.TestWorkflowEnvironment, attempts *int) {
				env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).
					Return(ProbeOutput{IsValidMedia: true, VideoWidth: 1920, VideoHeight: 1080}, nil).Once()
				env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).
					Return(func(_ context.Context, _ MediaInput, _ ProbeOutput) (DetectCropOutput, error) {
						*attempts++
						return DetectCropOutput{}, errors.New("crop failed")
					})
			},
		},
		{
			name:        "transcode is invoked exactly once on failure (no retries)",
			failingStep: TranscodeActivityName,
			setupMocks: func(env *testsuite.TestWorkflowEnvironment, attempts *int) {
				env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).
					Return(ProbeOutput{IsValidMedia: true, VideoWidth: 1920, VideoHeight: 1080}, nil).Once()
				env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).
					Return(DetectCropOutput{}, nil).Once()
				env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(func(_ context.Context, _ MediaInput, _ ProbeOutput, _ DetectCropOutput) (TranscodeOutput, error) {
						*attempts++
						return TranscodeOutput{}, errors.New("transcode failed")
					})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			newWorkflowActivities(t).Register(env)

			attempts := 0
			test.setupMocks(env, &attempts)

			var seenStep string

			env.OnActivity(NotifyFailureActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
				Return(func(_ context.Context, _ MediaInput, step, _ string) error {
					seenStep = step
					return nil
				}).Once()

			env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

			require.True(t, env.IsWorkflowCompleted())
			require.Error(t, env.GetWorkflowError())
			assert.Equal(t, 1, attempts, "%s should not be retried", test.failingStep)
			assert.Equal(t, test.failingStep, seenStep)
		})
	}
}

// TestTranscodeHeartbeatTimeout covers the helper that derives the
// HeartbeatTimeout for the transcode activity from the configured
// progress-log interval.
func TestTranscodeHeartbeatTimeout(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		expected time.Duration
	}{
		{
			name:     "zero interval disables heartbeat enforcement",
			interval: 0,
			expected: 0,
		},
		{
			name:     "negative interval disables heartbeat enforcement",
			interval: -5 * time.Second,
			expected: 0,
		},
		{
			name:     "default 30s progress interval yields 60s heartbeat timeout",
			interval: 30 * time.Second,
			expected: 60 * time.Second,
		},
		{
			name:     "5m progress interval yields 10m heartbeat timeout",
			interval: 5 * time.Minute,
			expected: 10 * time.Minute,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, transcodeHeartbeatTimeout(test.interval))
		})
	}
}

// TestMediaWorkflow_TranscodeHeartbeatTimeoutMatchesConfig verifies that the
// workflow's transcode ActivityOptions propagate a HeartbeatTimeout derived
// from the configured progress interval, so a stalled worker can be detected
// within minutes instead of waiting for StartToCloseTimeout to elapse.
func TestMediaWorkflow_TranscodeHeartbeatTimeoutMatchesConfig(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()

	progressInterval := 45 * time.Second

	a, err := NewActivities(
		MediaWorkflowConfig{
			DetectCropTimeout:   30 * time.Minute,
			TranscodeTimeout:    4 * time.Hour,
			ProgressLogInterval: progressInterval,
		},
		&stubLibraryClient{},
		&stubLibraryClient{},
		&webhook.Client{},
	)
	require.NoError(t, err)

	a.Register(env)

	probeOut := ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(DetectCropOutput{}, nil).Once()

	var capturedHeartbeatTimeout time.Duration

	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(func(ctx context.Context, _ MediaInput, _ ProbeOutput, _ DetectCropOutput) (TranscodeOutput, error) {
			capturedHeartbeatTimeout = activity.GetInfo(ctx).HeartbeatTimeout
			return TranscodeOutput{DestFilePath: "/out/file.mkv"}, nil
		}).Once()

	env.OnActivity(NotifyActivityName, mock.Anything, mock.Anything, mock.Anything).Return(NotifyOutput{}, nil).Once()
	env.OnActivity(CleanupActivityName, mock.Anything, mock.Anything, expectTranscode(TranscodeOutput{DestFilePath: "/out/file.mkv"}), mock.Anything).Return(nil).Once()

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, transcodeHeartbeatTimeout(progressInterval), capturedHeartbeatTimeout,
		"transcode activity should observe the HeartbeatTimeout derived from the configured progress interval")
}

// TestNotifyActivityOptions_BuildsRetryPolicyFromConfig verifies that
// notifyActivityOptions assembles the four-knob Temporal RetryPolicy from
// MediaWorkflowConfig (after NewActivities applies defaults to zero values),
// so operator-supplied env vars actually drive the retry behavior at the
// activity invocation site.
func TestNotifyActivityOptions_BuildsRetryPolicyFromConfig(t *testing.T) {
	tests := []struct {
		name     string
		cfg      MediaWorkflowConfig
		expected MediaWorkflowConfig
	}{
		{
			name: "zero values resolve to package defaults",
			cfg:  MediaWorkflowConfig{},
			expected: MediaWorkflowConfig{
				NotifyInitialInterval:    DefaultNotifyInitialInterval,
				NotifyBackoffCoefficient: DefaultNotifyBackoffCoefficient,
				NotifyMaximumInterval:    DefaultNotifyMaximumInterval,
				NotifyMaximumAttempts:    DefaultNotifyMaximumAttempts,
			},
		},
		{
			name: "operator overrides flow through to RetryPolicy",
			cfg: MediaWorkflowConfig{
				NotifyInitialInterval:    2 * time.Second,
				NotifyBackoffCoefficient: 2.0,
				NotifyMaximumInterval:    30 * time.Second,
				NotifyMaximumAttempts:    25,
			},
			expected: MediaWorkflowConfig{
				NotifyInitialInterval:    2 * time.Second,
				NotifyBackoffCoefficient: 2.0,
				NotifyMaximumInterval:    30 * time.Second,
				NotifyMaximumAttempts:    25,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, err := NewActivities(test.cfg, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
			require.NoError(t, err)

			opts := a.notifyActivityOptions()
			require.NotNil(t, opts.RetryPolicy)
			assert.Equal(t, test.expected.NotifyInitialInterval, opts.RetryPolicy.InitialInterval)
			assert.Equal(t, test.expected.NotifyBackoffCoefficient, opts.RetryPolicy.BackoffCoefficient)
			assert.Equal(t, test.expected.NotifyMaximumInterval, opts.RetryPolicy.MaximumInterval)
			assert.Equal(t, test.expected.NotifyMaximumAttempts, opts.RetryPolicy.MaximumAttempts)

			assert.Equal(t, ActivityTaskQueueByName(a.cfg.TaskQueuePrefix, NotifyActivityName), opts.TaskQueue)
			assert.Equal(t, defaultNotifyTimeout, opts.StartToCloseTimeout)
		})
	}
}

// stubLibraryClient lives in testhelpers_test.go and satisfies medialib.ArrLibrary
// for tests that do not exercise the live library calls.
var _ medialib.ArrLibrary = (*stubLibraryClient)(nil)

// expectTranscode builds a mock matcher that asserts the Cleanup activity
// received the exact TranscodeOutput the workflow should have passed at this
// call site. Equality matters here: the valid path must forward the real
// transcode result (so output-side pruning targets the right file), and the
// invalid path must pass a zero TranscodeOutput (so pruning is suppressed).
// Using mock.Anything would let either regression slip through.
func expectTranscode(want TranscodeOutput) any {
	return mock.MatchedBy(func(got TranscodeOutput) bool { return got == want })
}

// expectCrop matches the DetectCropOutput argument passed to the transcode
// activity. A zero-value DetectCropOutput (nil Crop) means no crop filter is
// applied, which is what the skip-crop-detection path must forward.
func expectCrop(want DetectCropOutput) any {
	return mock.MatchedBy(func(got DetectCropOutput) bool { return got == want })
}

// expectNotify builds a mock matcher that asserts the Cleanup activity received
// the exact NotifyOutput the workflow forwarded from Notify. Equality matters:
// the skip path must forward ImportSkipped so Cleanup removes the orphaned
// output, and the normal path must forward a zero value so it does not.
func expectNotify(want NotifyOutput) any {
	return mock.MatchedBy(func(got NotifyOutput) bool { return got == want })
}
