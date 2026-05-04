package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDRMEntry describes one entry under a synthetic /sys/class/drm/ tree:
// the entry's name (e.g. "renderD128", "card0"), and the basename of the
// driver the device/driver symlink should point to (empty means do not
// create the device/driver symlink at all).
type fakeDRMEntry struct {
	name   string
	driver string
}

// buildFakeDRMRoot creates a directory tree mimicking /sys/class/drm/ under
// t.TempDir(). Each entry becomes a directory containing a device/ subdir;
// when driver is non-empty, device/driver is a symlink to a sibling
// "drivers/<driver>" directory so os.Readlink returns the driver basename
// the way real sysfs does. Returns the absolute path of the synthesized DRM
// root.
func buildFakeDRMRoot(t *testing.T, entries []fakeDRMEntry) string {
	t.Helper()

	root := t.TempDir()
	driversDir := filepath.Join(root, "drivers")
	require.NoError(t, os.Mkdir(driversDir, 0o755))

	for _, entry := range entries {
		entryDir := filepath.Join(root, entry.name)
		require.NoError(t, os.Mkdir(entryDir, 0o755))

		deviceDir := filepath.Join(entryDir, "device")
		require.NoError(t, os.Mkdir(deviceDir, 0o755))

		if entry.driver == "" {
			continue
		}

		driverTarget := filepath.Join(driversDir, entry.driver)
		if err := os.MkdirAll(driverTarget, 0o755); err != nil && !os.IsExist(err) {
			require.NoError(t, err)
		}

		require.NoError(t, os.Symlink(driverTarget, filepath.Join(deviceDir, "driver")))
	}

	return root
}

func TestDetectI915RenderNode(t *testing.T) {
	tests := []struct {
		name     string
		entries  []fakeDRMEntry
		expected string
	}{
		{
			name: "single i915 render node returns its dev path",
			entries: []fakeDRMEntry{
				{name: "card0", driver: "i915"},
				{name: "renderD128", driver: "i915"},
			},
			expected: "/dev/dri/renderD128",
		},
		{
			name: "multiple render nodes returns the lowest-numbered i915",
			entries: []fakeDRMEntry{
				{name: "renderD129", driver: "i915"},
				{name: "renderD128", driver: "i915"},
				{name: "renderD130", driver: "i915"},
			},
			expected: "/dev/dri/renderD128",
		},
		{
			name:     "no DRM entries returns empty",
			entries:  nil,
			expected: "",
		},
		{
			name: "non-i915 drivers are ignored",
			entries: []fakeDRMEntry{
				{name: "renderD128", driver: "amdgpu"},
				{name: "renderD129", driver: "nouveau"},
			},
			expected: "",
		},
		{
			name: "card entries are ignored even when i915",
			entries: []fakeDRMEntry{
				{name: "card0", driver: "i915"},
			},
			expected: "",
		},
		{
			name: "render entry without device/driver symlink is skipped",
			entries: []fakeDRMEntry{
				{name: "renderD128", driver: ""},
				{name: "renderD129", driver: "i915"},
			},
			expected: "/dev/dri/renderD129",
		},
		{
			name: "non-numeric render suffix is skipped",
			entries: []fakeDRMEntry{
				{name: "renderDxyz", driver: "i915"},
				{name: "renderD200", driver: "i915"},
			},
			expected: "/dev/dri/renderD200",
		},
		{
			name: "mixed i915 and non-i915 picks lowest i915 only",
			entries: []fakeDRMEntry{
				{name: "renderD128", driver: "amdgpu"},
				{name: "renderD129", driver: "i915"},
				{name: "renderD130", driver: "i915"},
			},
			expected: "/dev/dri/renderD129",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := buildFakeDRMRoot(t, test.entries)

			got, err := detectI915RenderNode(root)
			require.NoError(t, err)
			assert.Equal(t, test.expected, got)
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
		root := buildFakeDRMRoot(t, []fakeDRMEntry{{name: "renderD128", driver: "i915"}})

		got := resolveHardwareDevicePath(context.Background(), "/dev/dri/custom", root)
		assert.Equal(t, "/dev/dri/custom", got)
	})

	t.Run("auto-detected path returned when no override", func(t *testing.T) {
		root := buildFakeDRMRoot(t, []fakeDRMEntry{{name: "renderD128", driver: "i915"}})

		got := resolveHardwareDevicePath(context.Background(), "", root)
		assert.Equal(t, "/dev/dri/renderD128", got)
	})

	t.Run("software-only when no override and no i915", func(t *testing.T) {
		root := buildFakeDRMRoot(t, []fakeDRMEntry{{name: "renderD128", driver: "amdgpu"}})

		got := resolveHardwareDevicePath(context.Background(), "", root)
		assert.Empty(t, got)
	})

	t.Run("software-only when DRM root is missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")

		got := resolveHardwareDevicePath(context.Background(), "", missing)
		assert.Empty(t, got)
	})
}
