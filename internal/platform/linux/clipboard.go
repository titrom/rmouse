//go:build linux

package linux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/titrom/rmouse/internal/proto"
)

// Clipboard implements platform.Clipboard on Linux by shelling out to
// xclip (preferred) or xsel. The X11 selection protocol (selection ownership,
// conversion requests, INCR transfers, MIME negotiation) is large enough that
// a third-party helper is far more robust than a from-scratch xgb
// implementation. Wayland is not supported here — the project's injector also
// requires X11, so both constraints align.
type Clipboard struct {
	tool clipTool

	mu       sync.Mutex
	lastHash [32]byte
	haveLast bool
}

type clipTool interface {
	// readTargets returns the MIME target list currently advertised by the
	// clipboard selection, lowercased.
	readTargets() ([]string, error)
	// read returns raw bytes for target; ok=false if the target is unavailable.
	read(target string) (data []byte, ok bool, err error)
	// write replaces the clipboard with data under the given target.
	write(target string, data []byte) error
}

func NewClipboard() (*Clipboard, error) {
	tool, err := detectClipboardTool()
	if err != nil {
		return nil, err
	}
	return &Clipboard{tool: tool}, nil
}

func detectClipboardTool() (clipTool, error) {
	if p, err := exec.LookPath("xclip"); err == nil {
		return xclipTool{path: p}, nil
	}
	if p, err := exec.LookPath("xsel"); err == nil {
		return xselTool{path: p}, nil
	}
	return nil, errors.New("platform/linux: neither xclip nor xsel found in PATH (install one: `sudo apt install xclip`)")
}

func (c *Clipboard) Read() (proto.ClipboardFormat, []byte, bool, error) {
	targets, err := c.tool.readTargets()
	if err != nil {
		return 0, nil, false, err
	}
	set := make(map[string]bool, len(targets))
	for _, t := range targets {
		set[t] = true
	}

	// Priority matches the Windows reader: files → image → text. A selection
	// with files also advertises text/plain (a textual path listing), so file
	// detection must come first to avoid replicating a filename as text.
	if set["text/uri-list"] || set["x-special/gnome-copied-files"] {
		if data, ok, err := c.readFilesList(set); err != nil {
			return 0, nil, false, err
		} else if ok {
			return proto.ClipboardFormatFilesList, data, true, nil
		}
	}
	if set["image/png"] {
		data, ok, err := c.tool.read("image/png")
		if err != nil {
			return 0, nil, false, err
		}
		if ok && len(data) > 0 {
			return proto.ClipboardFormatImagePNG, data, true, nil
		}
	}
	if set["utf8_string"] || set["text/plain;charset=utf-8"] || set["text/plain"] || set["string"] {
		target := "UTF8_STRING"
		if !set["utf8_string"] {
			if set["text/plain;charset=utf-8"] {
				target = "text/plain;charset=utf-8"
			} else if set["text/plain"] {
				target = "text/plain"
			} else {
				target = "STRING"
			}
		}
		data, ok, err := c.tool.read(target)
		if err != nil {
			return 0, nil, false, err
		}
		if ok {
			return proto.ClipboardFormatTextPlain, data, true, nil
		}
	}
	return 0, nil, false, nil
}

func (c *Clipboard) readFilesList(set map[string]bool) ([]byte, bool, error) {
	var raw []byte
	var ok bool
	var err error
	switch {
	case set["text/uri-list"]:
		raw, ok, err = c.tool.read("text/uri-list")
	case set["x-special/gnome-copied-files"]:
		raw, ok, err = c.tool.read("x-special/gnome-copied-files")
	}
	if err != nil || !ok {
		return nil, false, err
	}
	paths := parseURIList(raw)
	if len(paths) == 0 {
		return nil, false, nil
	}
	data, err := json.Marshal(paths)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// parseURIList converts a text/uri-list (or Nautilus' x-special/gnome-copied-files
// with a "copy\n" preamble) into absolute local paths. Lines starting with '#'
// are comments per RFC 2483. Only file:// URIs are accepted.
func parseURIList(raw []byte) []string {
	out := make([]string, 0, 4)
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// GNOME's "x-special/gnome-copied-files" starts with "copy" or "cut".
		if line == "copy" || line == "cut" {
			continue
		}
		if !strings.HasPrefix(line, "file://") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Path == "" {
			continue
		}
		out = append(out, u.Path)
	}
	return out
}

func (c *Clipboard) Write(format proto.ClipboardFormat, data []byte) error {
	if len(data) > proto.MaxClipboardData {
		return fmt.Errorf("clipboard payload too large: %d", len(data))
	}
	switch format {
	case proto.ClipboardFormatTextPlain:
		return c.tool.write("UTF8_STRING", data)
	case proto.ClipboardFormatImagePNG:
		return c.tool.write("image/png", data)
	case proto.ClipboardFormatFilesList:
		var paths []string
		if err := json.Unmarshal(data, &paths); err != nil {
			return fmt.Errorf("decode files list: %w", err)
		}
		if len(paths) == 0 {
			return errors.New("files list is empty")
		}
		var buf bytes.Buffer
		for _, p := range paths {
			buf.WriteString("file://")
			buf.WriteString((&url.URL{Path: p}).EscapedPath())
			buf.WriteString("\r\n")
		}
		return c.tool.write("text/uri-list", buf.Bytes())
	default:
		return fmt.Errorf("unsupported clipboard format: %d", format)
	}
}

func (c *Clipboard) Watch(ctx context.Context, sink func(proto.ClipboardFormat, []byte)) error {
	if sink == nil {
		return nil
	}
	// X11 offers no cheap "did the selection change" counter like Windows'
	// GetClipboardSequenceNumber, so we poll. 400 ms matches the latency users
	// accept on the Windows side (150 ms tick + sequence check) without
	// spawning xclip more than a couple times per second.
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			format, data, ok, err := c.Read()
			if err != nil || !ok {
				continue
			}
			h := hashClipboard(format, data)
			c.mu.Lock()
			if c.haveLast && h == c.lastHash {
				c.mu.Unlock()
				continue
			}
			c.lastHash = h
			c.haveLast = true
			c.mu.Unlock()
			sink(format, append([]byte(nil), data...))
		}
	}
}

func (*Clipboard) Close() error { return nil }

func hashClipboard(format proto.ClipboardFormat, data []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{byte(format)})
	h.Write(data)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// --- xclip backend -----------------------------------------------------

type xclipTool struct{ path string }

func (x xclipTool) readTargets() ([]string, error) {
	out, err := runCapture(x.path, "-selection", "clipboard", "-o", "-t", "TARGETS")
	if err != nil {
		// xclip returns non-zero on empty clipboard. Distinguish "no
		// selection owner" (exit 1, empty output) from a real failure by
		// trusting empty output.
		if len(out) == 0 {
			return nil, nil
		}
		return nil, err
	}
	return splitLower(out), nil
}

func (x xclipTool) read(target string) ([]byte, bool, error) {
	out, err := runCapture(x.path, "-selection", "clipboard", "-o", "-t", target)
	if err != nil {
		if len(out) == 0 {
			return nil, false, nil
		}
		return nil, false, err
	}
	return out, true, nil
}

func (x xclipTool) write(target string, data []byte) error {
	cmd := exec.Command(x.path, "-selection", "clipboard", "-i", "-t", target)
	cmd.Stdin = bytes.NewReader(data)
	// xclip forks into the background to keep owning the selection. Don't
	// block on Wait — start it, then release so it stays alive as the X11
	// selection owner until something replaces the clipboard.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("xclip start: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// --- xsel backend ------------------------------------------------------

type xselTool struct{ path string }

func (x xselTool) readTargets() ([]string, error) {
	// xsel exposes text only; treat it as if UTF8_STRING is always available
	// when the clipboard has any data and nothing else is.
	out, err := runCapture(x.path, "--clipboard", "--output")
	if err != nil || len(out) == 0 {
		return nil, nil
	}
	return []string{"utf8_string", "text/plain"}, nil
}

func (x xselTool) read(target string) ([]byte, bool, error) {
	t := strings.ToLower(target)
	if t != "utf8_string" && t != "text/plain" && t != "text/plain;charset=utf-8" && t != "string" {
		return nil, false, nil
	}
	out, err := runCapture(x.path, "--clipboard", "--output")
	if err != nil {
		return nil, false, err
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return out, true, nil
}

func (x xselTool) write(target string, data []byte) error {
	t := strings.ToLower(target)
	if t != "utf8_string" && t != "text/plain" && t != "text/plain;charset=utf-8" && t != "string" {
		return fmt.Errorf("xsel: unsupported target %q (install xclip for image/files support)", target)
	}
	cmd := exec.Command(x.path, "--clipboard", "--input")
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}

// --- helpers -----------------------------------------------------------

func runCapture(path string, args ...string) ([]byte, error) {
	cmd := exec.Command(path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func splitLower(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r", "")
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, strings.ToLower(p))
	}
	return out
}
