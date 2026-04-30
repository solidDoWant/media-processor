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

	var seenFinalize FinalizeInput

	env.OnActivity(FinalizeActivityName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, fin FinalizeInput) error {
			seenFinalize = fin
			return nil
		}).Once()

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, FinalizeValid, seenFinalize.Mode)
	assert.Equal(t, transOut.DestFilePath, seenFinalize.Transcode.DestFilePath)
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

func TestMediaWorkflow_FinalizeValidPathRetriesUpToThreeTimes(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	registerWorkflow(env, newWorkflowActivities(t))

	probeOut := steps.ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4", VideoWidth: 1920, VideoHeight: 1080}
	env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).Return(probeOut, nil).Once()
	env.OnActivity(DetectCropActivityName, mock.Anything, mock.Anything, mock.Anything).Return(steps.DetectCropOutput{}, nil).Once()
	env.OnActivity(TranscodeActivityName, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(steps.TranscodeOutput{DestFilePath: "/out/file.mkv"}, nil).Once()

	finalizeAttempts := 0

	env.OnActivity(FinalizeActivityName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, fin FinalizeInput) error {
			if fin.Mode != FinalizeValid {
				return nil
			}

			finalizeAttempts++
			if finalizeAttempts < 3 {
				return errors.New("transient finalize failure")
			}

			return nil
		}).Times(3)

	env.ExecuteWorkflow(MediaWorkflowName, MediaInput{FilePath: "/in/file.mp4", MediaType: medialib.MovieType, OutputPath: "/out"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "workflow should succeed when finalize succeeds within retry budget")
	assert.Equal(t, 3, finalizeAttempts, "valid-mode finalize should be invoked up to 3 times before succeeding")
}

func TestMediaWorkflow_NonRetryableActivitiesFailOnFirstError(t *testing.T) {
	tests := []struct {
		name         string
		failingStep  string
		expectedMode FinalizeMode
		setupMocks   func(env *testsuite.TestWorkflowEnvironment, attempts *int)
	}{
		{
			name:         "probe is invoked exactly once on failure (no retries)",
			failingStep:  "probe",
			expectedMode: FinalizeFailure,
			setupMocks: func(env *testsuite.TestWorkflowEnvironment, attempts *int) {
				env.OnActivity(ProbeActivityName, mock.Anything, mock.Anything).
					Return(func(_ context.Context, _ MediaInput) (steps.ProbeOutput, error) {
						*attempts++
						return steps.ProbeOutput{}, errors.New("probe failed")
					})
			},
		},
		{
			name:         "detectcrop is invoked exactly once on failure (no retries)",
			failingStep:  "detectcrop",
			expectedMode: FinalizeFailure,
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
			name:         "transcode is invoked exactly once on failure (no retries)",
			failingStep:  "transcode",
			expectedMode: FinalizeFailure,
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
			assert.Equal(t, test.expectedMode, seenFinalize.Mode)
			assert.Equal(t, test.failingStep, seenFinalize.FailureStep)
		})
	}
}

// stubLibraryClient lives in testhelpers_test.go and satisfies medialib.ArrLibrary
// for tests that do not exercise the live library calls.
var _ medialib.ArrLibrary = (*stubLibraryClient)(nil)
