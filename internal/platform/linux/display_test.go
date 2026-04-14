//go:build linux

package linux

import "testing"

func TestParseMonitorsDualHead(t *testing.T) {
	const sample = `Monitors: 2
 0: +*HDMI-A-0 1920/600x1080/340+0+0  HDMI-A-0
 1: +DP-0 2560/600x1440/340+1920+0  DP-0
`
	got, err := parseMonitors(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 monitors, got %d", len(got))
	}
	if got[0].W != 1920 || got[0].H != 1080 || got[0].X != 0 || got[0].Y != 0 || !got[0].Primary {
		t.Errorf("monitor 0 wrong: %#v", got[0])
	}
	if got[1].W != 2560 || got[1].H != 1440 || got[1].X != 1920 || got[1].Y != 0 || got[1].Primary {
		t.Errorf("monitor 1 wrong: %#v", got[1])
	}
	if got[0].Name != "HDMI-A-0" || got[1].Name != "DP-0" {
		t.Errorf("names wrong: %q %q", got[0].Name, got[1].Name)
	}
}

func TestParseMonitorsNegativeOffset(t *testing.T) {
	const sample = ` 0: +*DP-2 1920/600x1080/340+-1920+200  DP-2
`
	got, err := parseMonitors(sample)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].X != -1920 || got[0].Y != 200 {
		t.Errorf("negative offset not parsed: %#v", got[0])
	}
}

func TestParseMonitorsEmpty(t *testing.T) {
	if _, err := parseMonitors("Monitors: 0\n"); err == nil {
		t.Fatal("expected error on zero monitors")
	}
}
