//go:build windows

package windows

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/titrom/rmouse/internal/proto"
)

type Clipboard struct{}

func NewClipboard() (*Clipboard, error) { return &Clipboard{}, nil }

var (
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procIsClipboardFormatAvail     = user32.NewProc("IsClipboardFormatAvailable")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
	procGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	procGlobalFree                 = kernel32.NewProc("GlobalFree")
)

const (
	cfUnicodeText = 13
	cfHDrop       = 15
	cfDIB         = 8
	cfDIBV5       = 17

	gmemMoveable = 0x0002
	gmemZeroinit = 0x0040
)

type dropFiles struct {
	PFiles uint32
	PtX    int32
	PtY    int32
	FNC    uint32
	FWide  uint32
}

func (c *Clipboard) Read() (proto.ClipboardFormat, []byte, bool, error) {
	if err := openClipboardWithRetry(); err != nil {
		return 0, nil, false, err
	}
	defer closeClipboard()

	if ok, data, err := readFilesList(); err != nil {
		return 0, nil, false, err
	} else if ok {
		return proto.ClipboardFormatFilesList, data, true, nil
	}
	if ok, data, err := readPNGImage(); err != nil {
		return 0, nil, false, err
	} else if ok {
		return proto.ClipboardFormatImagePNG, data, true, nil
	}
	if ok, data, err := readText(); err != nil {
		return 0, nil, false, err
	} else if ok {
		return proto.ClipboardFormatTextPlain, data, true, nil
	}
	return 0, nil, false, nil
}

func (c *Clipboard) Write(format proto.ClipboardFormat, data []byte) error {
	if len(data) > proto.MaxClipboardData {
		return fmt.Errorf("clipboard payload too large: %d", len(data))
	}
	if err := openClipboardWithRetry(); err != nil {
		return err
	}
	defer closeClipboard()
	if r, _, errno := procEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("EmptyClipboard: %w", errno)
	}
	switch format {
	case proto.ClipboardFormatTextPlain:
		return writeText(data)
	case proto.ClipboardFormatImagePNG:
		return writePNGImage(data)
	case proto.ClipboardFormatFilesList:
		return writeFilesList(data)
	default:
		return fmt.Errorf("unsupported clipboard format: %d", format)
	}
}

func (c *Clipboard) Watch(ctx context.Context, sink func(format proto.ClipboardFormat, data []byte)) error {
	if sink == nil {
		return nil
	}
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	var last [32]byte
	var haveLast bool
	// Windows increments this counter on every clipboard change — cheaper
	// than doing a full Read (GlobalLock + memcopy + PNG re-encode) on every
	// tick just to hash the contents unchanged.
	var lastSeq uint32
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			seq := clipboardSequenceNumber()
			// seq==0 usually means the process briefly couldn't access the
			// clipboard; fall through to the Read (which retries on open)
			// so we don't silently miss an update.
			if seq != 0 && seq == lastSeq {
				continue
			}
			lastSeq = seq
			format, data, ok, err := c.Read()
			if err != nil || !ok {
				continue
			}
			h := hashClipboard(format, data)
			if haveLast && h == last {
				continue
			}
			last = h
			haveLast = true
			sink(format, append([]byte(nil), data...))
		}
	}
}

func clipboardSequenceNumber() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

func (c *Clipboard) Close() error { return nil }

func hashClipboard(format proto.ClipboardFormat, data []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{byte(format)})
	h.Write(data)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func openClipboardWithRetry() error {
	var last error
	for i := 0; i < 6; i++ {
		if r, _, errno := procOpenClipboard.Call(0); r != 0 {
			return nil
		} else {
			last = errno
			time.Sleep(time.Duration(20*(i+1)) * time.Millisecond)
		}
	}
	if last == nil {
		last = errors.New("unknown error")
	}
	return fmt.Errorf("OpenClipboard: %w", last)
}

func closeClipboard() {
	_, _, _ = procCloseClipboard.Call()
}

func formatAvailable(format uintptr) bool {
	r, _, _ := procIsClipboardFormatAvail.Call(format)
	return r != 0
}

func readText() (bool, []byte, error) {
	if !formatAvailable(cfUnicodeText) {
		return false, nil, nil
	}
	raw, err := readGlobalBytes(cfUnicodeText)
	if err != nil {
		return false, nil, err
	}
	u16 := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		v := binary.LittleEndian.Uint16(raw[i : i+2])
		if v == 0 {
			break
		}
		u16 = append(u16, v)
	}
	s := string(utf16.Decode(u16))
	return true, []byte(s), nil
}

func writeText(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("clipboard text is not valid UTF-8")
	}
	utf := utf16.Encode([]rune(string(data) + "\x00"))
	b := make([]byte, len(utf)*2)
	for i, v := range utf {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	h, err := allocGlobal(b)
	if err != nil {
		return err
	}
	if r, _, errno := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData(CF_UNICODETEXT): %w", errno)
	}
	return nil
}

func readFilesList() (bool, []byte, error) {
	if !formatAvailable(cfHDrop) {
		return false, nil, nil
	}
	raw, err := readGlobalBytes(cfHDrop)
	if err != nil {
		return false, nil, err
	}
	if len(raw) < int(unsafe.Sizeof(dropFiles{})) {
		return false, nil, errors.New("CF_HDROP payload too small")
	}
	h := (*dropFiles)(unsafe.Pointer(&raw[0]))
	if h.FWide == 0 {
		return false, nil, errors.New("CF_HDROP ANSI encoding is not supported")
	}
	start := int(h.PFiles)
	if start < 0 || start >= len(raw) {
		return false, nil, errors.New("CF_HDROP offset is out of bounds")
	}
	u16 := make([]uint16, 0, (len(raw)-start)/2)
	for i := start; i+1 < len(raw); i += 2 {
		u16 = append(u16, binary.LittleEndian.Uint16(raw[i:i+2]))
	}
	paths := parseMultiSZ(u16)
	if len(paths) == 0 {
		return false, nil, nil
	}
	data, err := json.Marshal(paths)
	if err != nil {
		return false, nil, err
	}
	return true, data, nil
}

func writeFilesList(data []byte) error {
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return fmt.Errorf("decode files list: %w", err)
	}
	if len(paths) == 0 {
		return errors.New("files list is empty")
	}
	sz := make([]uint16, 0, 64)
	for _, p := range paths {
		u := utf16.Encode([]rune(p))
		sz = append(sz, u...)
		sz = append(sz, 0)
	}
	sz = append(sz, 0)

	hdrSize := int(unsafe.Sizeof(dropFiles{}))
	buf := make([]byte, hdrSize+len(sz)*2)
	hdr := (*dropFiles)(unsafe.Pointer(&buf[0]))
	hdr.PFiles = uint32(hdrSize)
	hdr.FWide = 1
	pos := hdrSize
	for _, v := range sz {
		binary.LittleEndian.PutUint16(buf[pos:pos+2], v)
		pos += 2
	}
	h, err := allocGlobal(buf)
	if err != nil {
		return err
	}
	if r, _, errno := procSetClipboardData.Call(cfHDrop, h); r == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData(CF_HDROP): %w", errno)
	}
	return nil
}

func parseMultiSZ(src []uint16) []string {
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i < len(src); i++ {
		if src[i] != 0 {
			continue
		}
		if i == start {
			break
		}
		out = append(out, string(utf16.Decode(src[start:i])))
		start = i + 1
	}
	return out
}

func readPNGImage() (bool, []byte, error) {
	// Prefer CF_DIB: Windows auto-synthesizes it from CF_DIBV5 as a 40-byte
	// BITMAPINFOHEADER with BI_RGB compression, which our decoder handles
	// trivially. Parsing CF_DIBV5 directly means dealing with BI_BITFIELDS
	// and custom colour masks that many apps set.
	var formats = []uintptr{cfDIB, cfDIBV5}
	for _, f := range formats {
		if !formatAvailable(f) {
			continue
		}
		raw, err := readGlobalBytes(f)
		if err != nil {
			// Try the next format rather than aborting — a transient
			// GlobalLock failure on one format shouldn't drop the image.
			continue
		}
		pngData, convErr := dibToPNG(raw)
		if convErr == nil {
			return true, pngData, nil
		}
		// Remember the last conversion error; if every format fails, surface
		// it so upstream logging can point at the real cause.
		err = convErr
		_ = err
	}
	return false, nil, nil
}

func writePNGImage(data []byte) error {
	dib, err := pngToDIB(data)
	if err != nil {
		return err
	}
	h, err := allocGlobal(dib)
	if err != nil {
		return err
	}
	if r, _, errno := procSetClipboardData.Call(cfDIB, h); r == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData(CF_DIB): %w", errno)
	}
	return nil
}

func readGlobalBytes(format uintptr) ([]byte, error) {
	h, _, errno := procGetClipboardData.Call(format)
	if h == 0 {
		return nil, fmt.Errorf("GetClipboardData(%d): %w", format, errno)
	}
	ptr, _, errno := procGlobalLock.Call(h)
	if ptr == 0 {
		return nil, fmt.Errorf("GlobalLock(%d): %w", format, errno)
	}
	defer procGlobalUnlock.Call(h)
	sz, _, _ := procGlobalSize.Call(h)
	if sz == 0 {
		return nil, errors.New("GlobalSize returned zero")
	}
	//nolint:unsafeptr // Win32 GlobalLock returns a stable memory address for this scope.
	raw := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), int(sz))
	return append([]byte(nil), raw...), nil
}

func allocGlobal(data []byte) (uintptr, error) {
	h, _, errno := procGlobalAlloc.Call(gmemMoveable|gmemZeroinit, uintptr(len(data)))
	if h == 0 {
		return 0, fmt.Errorf("GlobalAlloc: %w", errno)
	}
	ptr, _, errno := procGlobalLock.Call(h)
	if ptr == 0 {
		procGlobalFree.Call(h)
		return 0, fmt.Errorf("GlobalLock: %w", errno)
	}
	//nolint:unsafeptr // Win32 GlobalLock returns a writable memory address for this scope.
	copy(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len(data)), data)
	procGlobalUnlock.Call(h)
	return h, nil
}

func dibToPNG(raw []byte) ([]byte, error) {
	if len(raw) < 40 {
		return nil, errors.New("DIB header is too small")
	}
	hdrSize := int(binary.LittleEndian.Uint32(raw[0:4]))
	if hdrSize < 40 || hdrSize > len(raw) {
		return nil, errors.New("invalid DIB header size")
	}
	w := int(int32(binary.LittleEndian.Uint32(raw[4:8])))
	hSigned := int32(binary.LittleEndian.Uint32(raw[8:12]))
	if w <= 0 || hSigned == 0 {
		return nil, errors.New("invalid DIB dimensions")
	}
	topDown := hSigned < 0
	h := int(hSigned)
	if h < 0 {
		h = -h
	}
	bpp := int(binary.LittleEndian.Uint16(raw[14:16]))
	comp := binary.LittleEndian.Uint32(raw[16:20])
	// BI_RGB (0) and BI_BITFIELDS (3) both cover uncompressed 24/32bpp
	// pixels in BGR/BGRA byte order for practically every image that lands
	// on the Windows clipboard from browsers, Paint, and screenshot tools.
	// BI_BITFIELDS technically allows arbitrary colour masks, but every
	// common emitter uses the standard R/G/B/A bytes, so decoding as BGRA
	// below yields the correct result.
	const biRGB, biBitFields = 0, 3
	if comp != biRGB && comp != biBitFields {
		return nil, fmt.Errorf("unsupported DIB compression: %d", comp)
	}
	if bpp != 24 && bpp != 32 {
		return nil, fmt.Errorf("unsupported DIB bpp: %d", bpp)
	}
	pixelOffset := hdrSize
	stride := ((w*bpp + 31) / 32) * 4
	need := pixelOffset + stride*h
	if need > len(raw) {
		return nil, errors.New("truncated DIB pixel data")
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	// For 32bpp BI_RGB many Windows apps (Paint, Snipping Tool) leave the
	// alpha byte zero across the whole image — round-tripping through PNG
	// would produce a fully transparent result. Track whether any non-zero
	// alpha was seen; if not, treat the image as opaque.
	anyAlpha := bpp != 32
	for y := 0; y < h; y++ {
		srcY := y
		if !topDown {
			srcY = h - 1 - y
		}
		row := raw[pixelOffset+srcY*stride:]
		for x := 0; x < w; x++ {
			i := x * (bpp / 8)
			b, g, r := row[i], row[i+1], row[i+2]
			a := uint8(255)
			if bpp == 32 {
				a = row[i+3]
				if a != 0 {
					anyAlpha = true
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
		}
	}
	if !anyAlpha {
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 255
		}
	}
	var out bytes.Buffer
	// BestSpeed keeps the clipboard watcher responsive for screenshot-sized
	// images; the default (level 6) can take 300–800 ms on a 4K capture,
	// which manifests as visible lag before an item appears in the history
	// panel or reaches peers.
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func pngToDIB(data []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode png: %w", err)
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return nil, errors.New("empty image")
	}
	stride := w * 4
	pixels := make([]byte, stride*h)
	for y := 0; y < h; y++ {
		dstY := h - 1 - y // DIB default is bottom-up.
		row := pixels[dstY*stride:]
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(img.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			i := x * 4
			row[i+0] = c.B
			row[i+1] = c.G
			row[i+2] = c.R
			row[i+3] = c.A
		}
	}
	hdr := make([]byte, 40)
	binary.LittleEndian.PutUint32(hdr[0:4], 40)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(w))
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(h))
	binary.LittleEndian.PutUint16(hdr[12:14], 1)
	binary.LittleEndian.PutUint16(hdr[14:16], 32)
	binary.LittleEndian.PutUint32(hdr[16:20], 0) // BI_RGB
	binary.LittleEndian.PutUint32(hdr[20:24], uint32(len(pixels)))
	return append(hdr, pixels...), nil
}
