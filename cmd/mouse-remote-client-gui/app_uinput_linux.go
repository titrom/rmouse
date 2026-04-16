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
	// What this snippet does, in order:
	//   1. Persistent udev rule (without static_node — that one bites us:
	//      the prebuilt node opens fine but every ioctl returns EINVAL).
	//   2. usermod for future logins.
	//   3. modprobe uinput. Does nothing if it's compiled into the kernel.
	//   4. Read the real (major:minor) from /sys/devices/virtual/misc/uinput/dev,
	//      then `rm -f /dev/uinput && mknod ... c MAJOR MINOR`. This wipes
	//      any stale static-node from a previous rule and re-binds /dev/uinput
	//      to the live driver.
	//   5. chown to the calling user + chmod 0600 for immediate access.
	script := fmt.Sprintf(`set -e
USERNAME=%s
mkdir -p /etc/udev/rules.d
cat > /etc/udev/rules.d/99-uinput.rules <<'EOF'
KERNEL=="uinput", GROUP="input", MODE="0660"
EOF
udevadm control --reload-rules || true
usermod -aG input "$USERNAME" || true
modprobe uinput >/dev/null 2>&1 || true
# Wait briefly for the misc device to appear.
for i in 1 2 3 4 5; do
  [ -r /sys/devices/virtual/misc/uinput/dev ] && break
  sleep 0.2
done
if [ -r /sys/devices/virtual/misc/uinput/dev ]; then
  MM=$(cat /sys/devices/virtual/misc/uinput/dev)
  MAJOR=${MM%%:*}
  MINOR=${MM##*:}
  rm -f /dev/uinput
  mknod /dev/uinput c "$MAJOR" "$MINOR"
  chown "$USERNAME" /dev/uinput
  chmod 0600 /dev/uinput
else
  # Fallback if /sys path not available — try the canonical pair and
  # fix up perms if a node already exists.
  if [ ! -e /dev/uinput ]; then
    mknod /dev/uinput c 10 223 || true
  fi
  chown "$USERNAME" /dev/uinput || true
  chmod 0600 /dev/uinput || true
fi
`, shellQuote(username))

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
