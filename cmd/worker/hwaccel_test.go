package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDRMEntry describes one entry under a synthetic /sys/class/drm/ tree:
// the entry's name (e.g. "renderD128", "card0"), the basename of the driver
// the device/driver symlink should point to (empty means do not create the
// device/driver symlink at all), and whether a corresponding regular file
// should be created under the synthesized /dev/dri root so the detector's
// device-existence check passes.
type fakeDRMEntry struct {
	name        string
	driver      string
	createDevDR bool
}

// buildFakeDRMRoot creates a directory tree mimicking /sys/class/drm/ and a
// matching /dev/dri/ under t.TempDir(). Each DRM entry becomes a directory
// containing a device/ subdir; when driver is non-empty, device/driver is a
// symlink to a sibling "drivers/<driver>" directory so os.Readlink returns
// the driver basename the way real sysfs does. When createDevDR is true a
// matching regular file is created under the dev root so the swapped-in
// existence-only validateDevice stub treats the candidate as present. The
// production devDRIRoot and validateDevice are swapped out for the duration
// of the test and restored via t.Cleanup. Returns the absolute path of the
// synthesized DRM root.
func buildFakeDRMRoot(t *testing.T, entries []fakeDRMEntry) string {
	t.Helper()

	root := t.TempDir()
	driversDir := filepath.Join(root, "drivers")
	require.NoError(t, os.Mkdir(driversDir, 0o755))

	devRoot := t.TempDir()

	originalDevRoot := devDRIRoot
	originalValidator := validateDevice

	devDRIRoot = devRoot
	validateDevice = func(path string) error {
		_, err := os.Stat(path)

		return err
	}

	t.Cleanup(func() {
		devDRIRoot = originalDevRoot
		validateDevice = originalValidator
	})

	for _, entry := range entries {
		entryDir := filepath.Join(root, entry.name)
		require.NoError(t, os.Mkdir(entryDir, 0o755))

		deviceDir := filepath.Join(entryDir, "device")
		require.NoError(t, os.Mkdir(deviceDir, 0o755))

		if entry.driver != "" {
			driverTarget := filepath.Join(driversDir, entry.driver)
			if err := os.MkdirAll(driverTarget, 0o755); err != nil && !os.IsExist(err) {
				require.NoError(t, err)
			}

			require.NoError(t, os.Symlink(driverTarget, filepath.Join(deviceDir, "driver")))
		}

		if entry.createDevDR {
			f, err := os.Create(filepath.Join(devRoot, entry.name))
			require.NoError(t, err)
			require.NoError(t, f.Close())
		}
	}

	return root
}

func TestDetectI915RenderNode(t *testing.T) {
	tests := []struct {
		name        string
		entries     []fakeDRMEntry
		expectedDev string
		errFunc     require.ErrorAssertionFunc
	}{
		{
			name: "single i915 render node returns its dev path",
			entries: []fakeDRMEntry{
				{name: "card0", driver: "i915"},
				{name: "renderD128", driver: "i915", createDevDR: true},
			},
			expectedDev: "renderD128",
		},
		{
			name: "multiple render nodes returns the lowest-numbered i915",
			entries: []fakeDRMEntry{
				{name: "renderD129", driver: "i915", createDevDR: true},
				{name: "renderD128", driver: "i915", createDevDR: true},
				{name: "renderD130", driver: "i915", createDevDR: true},
			},
			expectedDev: "renderD128",
		},
		{
			name:    "no DRM entries returns empty",
			entries: nil,
		},
		{
			name: "non-i915 drivers are ignored",
			entries: []fakeDRMEntry{
				{name: "renderD128", driver: "amdgpu", createDevDR: true},
				{name: "renderD129", driver: "nouveau", createDevDR: true},
			},
		},
		{
			name: "card entries are ignored even when i915",
			entries: []fakeDRMEntry{
				{name: "card0", driver: "i915"},
			},
		},
		{
			name: "render entry without device/driver symlink is skipped",
			entries: []fakeDRMEntry{
				{name: "renderD128", driver: "", createDevDR: true},
				{name: "renderD129", driver: "i915", createDevDR: true},
			},
			expectedDev: "renderD129",
		},
		{
			name: "non-numeric render suffix is skipped",
			entries: []fakeDRMEntry{
				{name: "renderDxyz", driver: "i915", createDevDR: true},
				{name: "renderD200", driver: "i915", createDevDR: true},
			},
			expectedDev: "renderD200",
		},
		{
			name: "mixed i915 and non-i915 picks lowest i915 only",
			entries: []fakeDRMEntry{
				{name: "renderD128", driver: "amdgpu", createDevDR: true},
				{name: "renderD129", driver: "i915", createDevDR: true},
				{name: "renderD130", driver: "i915", createDevDR: true},
			},
			expectedDev: "renderD129",
		},
		{
			name: "i915 sysfs without matching /dev/dri node falls back",
			entries: []fakeDRMEntry{
				{name: "renderD128", driver: "i915"},
				{name: "renderD129", driver: "i915", createDevDR: true},
			},
			expectedDev: "renderD129",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := buildFakeDRMRoot(t, test.entries)

			got, err := detectI915RenderNode(root)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)

			var expected string
			if test.expectedDev != "" {
				expected = filepath.Join(devDRIRoot, test.expectedDev)
			}

			assert.Equal(t, expected, got)
		})
	}
}

// TestDetectI915RenderNode_MissingRoot verifies that a missing DRM root
// (e.g. on non-Linux hosts or minimal containers) is treated as "no GPU
// detected" rather than an error so the worker can still start in
// software-only mode.
func TestDetectI915RenderNode_MissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	got, err := detectI915RenderNode(missing)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestResolveHardwareDevicePath(t *testing.T) {
	t.Run("override wins over detection", func(t *testing.T) {
		root := buildFakeDRMRoot(t, []fakeDRMEntry{{name: "renderD128", driver: "i915", createDevDR: true}})

		got := resolveHardwareDevicePath(t.Context(), "/dev/dri/custom", root)
		assert.Equal(t, "/dev/dri/custom", got)
	})

	t.Run("auto-detected path returned when no override", func(t *testing.T) {
		root := buildFakeDRMRoot(t, []fakeDRMEntry{{name: "renderD128", driver: "i915", createDevDR: true}})

		got := resolveHardwareDevicePath(t.Context(), "", root)
		assert.Equal(t, filepath.Join(devDRIRoot, "renderD128"), got)
	})

	t.Run("software-only when no override and no i915", func(t *testing.T) {
		root := buildFakeDRMRoot(t, []fakeDRMEntry{{name: "renderD128", driver: "amdgpu", createDevDR: true}})

		got := resolveHardwareDevicePath(t.Context(), "", root)
		assert.Empty(t, got)
	})

	t.Run("software-only when DRM root is missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")

		got := resolveHardwareDevicePath(t.Context(), "", missing)
		assert.Empty(t, got)
	})
}
