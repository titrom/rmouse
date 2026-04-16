//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
)

func hasUinputAccess() bool {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// requestUinputAccess runs a small bootstrap script under pkexec. The
// script must be self-contained because pkexec swaps to root with a
// scrubbed environment.
func requestUinputAccess() error {
	if hasUinputAccess() {
		return nil
	}
	if _, err := exec.LookPath("pkexec"); err != nil {
		return fmt.Errorf("pkexec not found — install polkit, or run: " +
			"sudo usermod -aG input $USER && sudo chmod 0666 /dev/uinput")
	}
	username := os.Getenv("USER")
	if username == "" {
		if u, err := user.Current(); err == nil {
			username = u.Username
		}
	}
	if username == "" {
		return fmt.Errorf("could not determine current user")
	}
	// One self-contained snippet under pkexec:
	//   - persistent udev rule for the next reboot
	//   - usermod for future logins
	//   - modprobe to make sure /dev/uinput exists right now
	//   - udevadm settle, NOT trigger: trigger is async and races with the
	//     chmod/chown below — udev finishes after we exit and resets the
	//     mode back to 0660 root:input, leaving us locked out again.
	//   - chown the live device to the calling user so this very process
	//     can rw it before logging out.
	script := fmt.Sprintf(`set -e
mkdir -p /etc/udev/rules.d
cat > /etc/udev/rules.d/99-uinput.rules <<'EOF'
KERNEL=="uinput", GROUP="input", MODE="0660", OPTIONS+="static_node=uinput"
EOF
udevadm control --reload-rules || true
usermod -aG input %s || true
modprobe uinput || true
udevadm settle || true
if [ -e /dev/uinput ]; then
  chown %s /dev/uinput || true
  chmod 0600 /dev/uinput || true
fi
`, shellQuote(username), shellQuote(username))

	cmd := exec.Command("pkexec", "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pkexec failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if !hasUinputAccess() {
		return fmt.Errorf("granted, but /dev/uinput is still not writable — try logging out and back in")
	}
	return nil
}

// shellQuote single-quotes a string for safe inclusion in a /bin/sh
// command line. Single quotes inside the input are escaped.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
