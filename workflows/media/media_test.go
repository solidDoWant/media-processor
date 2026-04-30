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
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// registerWorkflow wires the workflow + four activities into the test
// environment. Mirrors Activities.Register, which targets a real worker.Worker.
func registerWorkflow(env *testsuite.TestWorkflowEnvironment, a *Activities) {
	env.RegisterWorkflowWithOptions(a.MediaWorkflow, workflow.RegisterOptions{Name: MediaWorkflowName})
	env.RegisterActivityWithOptions(a.Probe, activity.RegisterOptions{Name: ProbeActivityName})
	env.RegisterActivityWithOptions(a.DetectCrop, activity.RegisterOptions{Name: DetectCropActivityName})
	env.RegisterActivityWithOptions(a.Transcode, activity.RegisterOptions{Name: TranscodeActivityName})
	env.RegisterActivityWithOptions(a.Finalize, activity.RegisterOptions{Name: FinalizeActivityName})
}

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
	registerWorkflow(env, newWorkflowActivities(t))

	probeOut := steps.ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080, StartedAt: time.Now()}
	cropOut := steps.DetectCropOutput{}
	transOut := steps.TranscodeOutput{DestCodec: "hevc", DestContainer: "mkv", DestFilePath: "/out/file.mkv"}

	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(cropOut, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(transOut, nil).Once()

	var seenModes []FinalizeMode

	env.OnActivity(FinalizeActivityName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, fin FinalizeInput) error {
			seenModes = append(seenModes, fin.Mode)
			return nil
		}).Times(3)

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	// Valid path: notify, then cleanup, then metrics — each is its own
	// Finalize invocation with mode-appropriate retry options.
	assert.Equal(t, []FinalizeMode{FinalizeNotify, FinalizeCleanup, FinalizeMetrics}, seenModes)
	env.AssertExpectations(t)
}

func TestMediaWorkflow_InvalidPath_SkipsTranscodeAndCallsFinalizeInvalid(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	registerWorkflow(env, newWorkflowActivities(t))

	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).
		Return(steps.ProbeOutput{IsValidMedia: false}, nil).Once()

	var seenFinalize FinalizeInput

	env.OnActivity(FinalizeActivityName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, fin FinalizeInput) error {
			seenFinalize = fin
			return nil
		}).Once()

	// DetectCrop and Transcode must NOT be invoked. The mock fails the test if
	// any unexpected call arrives because no .Return was registered for them.

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.txt", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, FinalizeInvalid, seenFinalize.Mode)
	env.AssertExpectations(t)
}

func TestMediaWorkflow_TranscodeFailureFiresFailureWebhook(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	registerWorkflow(env, newWorkflowActivities(t))

	probeOut := steps.ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(steps.DetectCropOutput{}, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(steps.TranscodeOutput{}, errors.New("ffmpeg blew up")).Once()

	var seenFinalize FinalizeInput

	env.OnActivity(FinalizeActivityName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, fin FinalizeInput) error {
			seenFinalize = fin
			return nil
		}).Once()

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError(), "workflow should propagate the activity failure")

	assert.Equal(t, FinalizeFailure, seenFinalize.Mode)
	assert.Equal(t, "transcode", seenFinalize.FailureStep)
	assert.Contains(t, seenFinalize.FailureErr, "ffmpeg blew up")
	env.AssertExpectations(t)
}

func TestMediaWorkflow_ProbeFailureFiresFailureWebhook(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	registerWorkflow(env, newWorkflowActivities(t))

	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).
		Return(steps.ProbeOutput{}, errors.New("probe failed")).Once()

	var seenFinalize FinalizeInput

	env.OnActivity(FinalizeActivityName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, fin FinalizeInput) error {
			seenFinalize = fin
			return nil
		}).Once()

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	assert.Equal(t, FinalizeFailure, seenFinalize.Mode)
	assert.Equal(t, "probe", seenFinalize.FailureStep)
}

// TestMediaWorkflow_FinalizeNotifyAndCleanupRetry verifies that the notify and
// cleanup sub-modes each retry up to 3 times when their first attempts fail
// transiently, and that metrics still emits exactly once afterwards.
func TestMediaWorkflow_FinalizeNotifyAndCleanupRetry(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	registerWorkflow(env, newWorkflowActivities(t))

	probeOut := steps.ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(steps.DetectCropOutput{}, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(steps.TranscodeOutput{DestFilePath: "/out/file.mkv"}, nil).Once()

	notifyAttempts := 0
	cleanupAttempts := 0
	metricsAttempts := 0

	env.OnActivity(FinalizeActivityName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, fin FinalizeInput) error {
			switch fin.Mode {
			case FinalizeNotify:
				notifyAttempts++
				if notifyAttempts < 3 {
					return errors.New("transient notify failure")
				}
			case FinalizeCleanup:
				cleanupAttempts++
				if cleanupAttempts < 3 {
					return errors.New("transient cleanup failure")
				}
			case FinalizeMetrics:
				metricsAttempts++
			}

			return nil
		})

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, 3, notifyAttempts, "notify should retry up to 3 times")
	assert.Equal(t, 3, cleanupAttempts, "cleanup should retry up to 3 times")
	assert.Equal(t, 1, metricsAttempts, "metrics should fire exactly once after the retried steps succeed")
}

// TestMediaWorkflow_FinalizeMetricsDoesNotRetryAndDoesNotFailWorkflow verifies
// that a metrics activity error does not propagate to the workflow result and
// that the activity is invoked exactly once. Histogram emission is not
// idempotent, so retries would double-count.
func TestMediaWorkflow_FinalizeMetricsDoesNotRetryAndDoesNotFailWorkflow(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	registerWorkflow(env, newWorkflowActivities(t))

	probeOut := steps.ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(steps.DetectCropOutput{}, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(steps.TranscodeOutput{DestFilePath: "/out/file.mkv"}, nil).Once()

	metricsAttempts := 0
	finalizeFailureCalled := false

	env.OnActivity(FinalizeActivityName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, fin FinalizeInput) error {
			switch fin.Mode {
			case FinalizeNotify, FinalizeCleanup:
				return nil
			case FinalizeMetrics:
				metricsAttempts++
				return errors.New("OTel sdk panic")
			case FinalizeFailure:
				finalizeFailureCalled = true
			}

			return nil
		})

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "metrics failure must not fail the workflow")
	assert.Equal(t, 1, metricsAttempts, "metrics activity should not be retried")
	assert.False(t, finalizeFailureCalled, "failure-webhook must not fire when only metrics fail")
}

// TestMediaWorkflow_NonRetryableInputErrorOnNotifyDoesNotRetry verifies that
// the workflow does not burn the notify retry budget on a pure-data error
// (unknown media type). The non-retryable ApplicationError stops Temporal at
// the first attempt.
func TestMediaWorkflow_NonRetryableInputErrorOnNotifyDoesNotRetry(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	registerWorkflow(env, newWorkflowActivities(t))

	probeOut := steps.ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(steps.DetectCropOutput{}, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(steps.TranscodeOutput{DestFilePath: "/out/file.mkv"}, nil).Once()

	notifyAttempts := 0

	env.OnActivity(FinalizeActivityName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, fin FinalizeInput) error {
			switch fin.Mode {
			case FinalizeNotify:
				notifyAttempts++
				return temporal.NewNonRetryableApplicationError("unknown media type", errTypeNonRetryable, nil)
			case FinalizeFailure:
				return nil
			}

			return nil
		})

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
					Return(func(_ context.Context, _ MediaInput) (steps.ProbeOutput, error) {
						*attempts++
						return steps.ProbeOutput{}, errors.New("probe failed")
					})
			},
		},
		{
			name:        "detectcrop is invoked exactly once on failure (no retries)",
			failingStep: "detectcrop",
			setupMocks: func(env *testsuite.TestWorkflowEnvironment, attempts *int) {
				env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).
					Return(steps.ProbeOutput{IsValidMedia: true, VideoWidth: 1920, VideoHeight: 1080}, nil).Once()
				env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).
					Return(func(_ context.Context, _ MediaInput, _ steps.ProbeOutput) (steps.DetectCropOutput, error) {
						*attempts++
						return steps.DetectCropOutput{}, errors.New("crop failed")
					})
			},
		},
		{
			name:        "transcode is invoked exactly once on failure (no retries)",
			failingStep: "transcode",
			setupMocks: func(env *testsuite.TestWorkflowEnvironment, attempts *int) {
				env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).
					Return(steps.ProbeOutput{IsValidMedia: true, VideoWidth: 1920, VideoHeight: 1080}, nil).Once()
				env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).
					Return(steps.DetectCropOutput{}, nil).Once()
				env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(func(_ context.Context, _ MediaInput, _ steps.ProbeOutput, _ steps.DetectCropOutput) (steps.TranscodeOutput, error) {
						*attempts++
						return steps.TranscodeOutput{}, errors.New("transcode failed")
					})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			registerWorkflow(env, newWorkflowActivities(t))

			attempts := 0
			test.setupMocks(env, &attempts)

			var seenFinalize FinalizeInput

			env.OnActivity(FinalizeActivityName, mock.Anything, mock.Anything).
				Return(func(_ context.Context, fin FinalizeInput) error {
					seenFinalize = fin
					return nil
				}).Once()

			env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

			require.True(t, env.IsWorkflowCompleted())
			require.Error(t, env.GetWorkflowError())
			assert.Equal(t, 1, attempts, "%s should not be retried", test.failingStep)
			assert.Equal(t, FinalizeFailure, seenFinalize.Mode)
			assert.Equal(t, test.failingStep, seenFinalize.FailureStep)
		})
	}
}

// stubLibraryClient lives in testhelpers_test.go and satisfies medialib.ArrLibrary
// for tests that do not exercise the live library calls.
var _ medialib.ArrLibrary = (*stubLibraryClient)(nil)
