//go:build linux

// Package linux implements the platform.Display interface on Linux by shelling
// out to `xrandr --listactivemonitors`. This avoids pulling in a heavyweight
// X11 binding dependency and is adequate for MVP: one-shot enumeration is all
// we need before layout and edge-crossing are built in M4.
package linux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"

	"github.com/titrom/rmouse/internal/proto"
)

type Display struct{}

func New() *Display { return &Display{} }

// ErrHotplugUnsupported is returned by Subscribe — XRandR-based hotplug will
// be added later; for MVP users can restart the client when changing displays.
var ErrHotplugUnsupported = errors.New("platform/linux: hotplug subscription not yet implemented")

// Format of `xrandr --listactivemonitors` (excerpt):
//   Monitors: 2
//    0: +*HDMI-A-0 1920/600x1080/340+0+0  HDMI-A-0
//    1: +DP-0      2560/600x1440/340+1920+0  DP-0
//
// We only need: index, primary flag (`*`), W, H, X, Y, Name.
var monitorLine = regexp.MustCompile(`^\s*(\d+):\s+\+(\*)?\S*\s+(\d+)/\d+x(\d+)/\d+\+(-?\d+)\+(-?\d+)\s+(\S+)`)

func (*Display) Enumerate() ([]proto.Monitor, error) {
	cmd := exec.Command("xrandr", "--listactivemonitors")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("xrandr: %w (is X11 running?)", err)
	}
	return parseMonitors(string(out))
}

func parseMonitors(xrandrOutput string) ([]proto.Monitor, error) {
	var monitors []proto.Monitor
	for _, line := range splitLines(xrandrOutput) {
		m := monitorLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		w, _ := strconv.Atoi(m[3])
		h, _ := strconv.Atoi(m[4])
		x, _ := strconv.Atoi(m[5])
		y, _ := strconv.Atoi(m[6])
		monitors = append(monitors, proto.Monitor{
			ID:      uint8(idx),
			X:       int32(x),
			Y:       int32(y),
			W:       uint32(w),
			H:       uint32(h),
			Primary: m[2] == "*",
			Name:    m[7],
		})
	}
	if len(monitors) == 0 {
		return nil, errors.New("platform/linux: no monitors parsed from xrandr output")
	}
	return monitors, nil
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func (*Display) Subscribe(ctx context.Context, ch chan<- []proto.Monitor) error {
	return ErrHotplugUnsupported
}
