package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
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
	env.OnActivity(NotifyActivityName, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(CleanupActivityName, mock.Anything, mock.Anything).Return(nil).Once()

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

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
	env.OnActivity(CleanupActivityName, mock.Anything, mock.Anything).Return(nil).Once()

	// DetectCrop, Transcode, and Notify must NOT be invoked. The mock fails
	// the test if any unexpected call arrives because no .Return was
	// registered for them.

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.txt", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
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
	assert.Equal(t, "transcode", seenStep)
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
	assert.Equal(t, "probe", seenStep)
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
		Return(func(_ context.Context, _ MediaInput, _ TranscodeOutput) error {
			notifyAttempts++
			if notifyAttempts < 3 {
				return errors.New("transient notify failure")
			}

			return nil
		})

	env.OnActivity(CleanupActivityName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ MediaInput) error {
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
		Return(func(_ context.Context, _ MediaInput, _ TranscodeOutput) error {
			notifyAttempts++
			return temporal.NewNonRetryableApplicationError("unknown media type", errTypeNonRetryable, nil)
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
			failingStep: "probe",
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
			failingStep: "detectcrop",
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
			failingStep: "transcode",
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

// stubLibraryClient lives in testhelpers_test.go and satisfies medialib.ArrLibrary
// for tests that do not exercise the live library calls.
var _ medialib.ArrLibrary = (*stubLibraryClient)(nil)
