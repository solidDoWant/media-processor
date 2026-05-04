package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// defaultDRMRoot is the kernel sysfs path that lists DRM devices on Linux.
// Tests pass an alternate root populated with a fake layout under t.TempDir().
const defaultDRMRoot = "/sys/class/drm"

// devDRIRoot is the device-node directory render paths are reported under.
// Pulled out so tests can verify the returned path without hard-coding it.
const devDRIRoot = "/dev/dri"

// detectI915RenderNode scans drmRoot for render nodes whose backing driver is
// the Intel i915 kernel module and returns the /dev/dri path of the
// lowest-numbered match. Returns the empty string when no i915 render node is
// found. An I/O error on drmRoot itself is returned; per-entry stat/readlink
// failures are skipped so a partial sysfs view doesn't fail detection. A
// missing drmRoot is treated as "no GPU detected" rather than an error so
// non-Linux hosts and minimal containers don't fail the orchestration.
func detectI915RenderNode(drmRoot string) (string, error) {
	entries, err := os.ReadDir(drmRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}

		return "", fmt.Errorf("read DRM root %q: %w", drmRoot, err)
	}

	type renderMatch struct {
		name string
		num  int
	}

	var matches []renderMatch

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "renderD") {
			continue
		}

		num, parseErr := strconv.Atoi(strings.TrimPrefix(name, "renderD"))
		if parseErr != nil {
			continue
		}

		driverPath, readErr := os.Readlink(filepath.Join(drmRoot, name, "device", "driver"))
		if readErr != nil {
			continue
		}

		if filepath.Base(driverPath) != "i915" {
			continue
		}

		matches = append(matches, renderMatch{name: name, num: num})
	}

	if len(matches) == 0 {
		return "", nil
	}

	sort.Slice(matches, func(i, j int) bool { return matches[i].num < matches[j].num })

	return filepath.Join(devDRIRoot, matches[0].name), nil
}

// resolveHardwareDevicePath returns the device path the worker should pass to
// the transcode activity. When override is non-empty it wins outright (no
// auto-detection runs); otherwise drmRoot is scanned for an i915 render node.
// The chosen path (or its absence) is logged with a marker indicating the
// source so operators can tell at a glance whether the worker is using their
// supplied path, an auto-detected one, or running in software-only mode.
func resolveHardwareDevicePath(ctx context.Context, override, drmRoot string) string {
	if override != "" {
		slog.InfoContext(ctx, "using operator-supplied hardware device path",
			slog.String("source", "override"),
			slog.String("path", override),
		)

		return override
	}

	detected, err := detectI915RenderNode(drmRoot)
	if err != nil {
		slog.WarnContext(ctx, "hardware device auto-detection failed; running in software-only mode",
			slog.Any("error", err),
		)

		return ""
	}

	if detected == "" {
		slog.InfoContext(ctx, "no Intel GPU detected — software-only mode")
		return ""
	}

	slog.InfoContext(ctx, "auto-detected Intel i915 render node",
		slog.String("source", "auto-detected"),
		slog.String("path", detected),
	)

	return detected
}
