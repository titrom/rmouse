//go:build windows

package windows

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestParseMultiSZ(t *testing.T) {
	// "C:\a.txt\0D:\b.png\0\0"
	src := []uint16{
		67, 58, 92, 97, 46, 116, 120, 116, 0,
		68, 58, 92, 98, 46, 112, 110, 103, 0,
		0,
	}
	got := parseMultiSZ(src)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0] != `C:\a.txt` || got[1] != `D:\b.png` {
		t.Fatalf("unexpected parse result: %#v", got)
	}
}

func TestPNGDIBRoundTrip(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	img.SetNRGBA(1, 0, color.NRGBA{R: 0, G: 255, B: 0, A: 255})
	img.SetNRGBA(2, 0, color.NRGBA{R: 0, G: 0, B: 255, A: 255})
	img.SetNRGBA(0, 1, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	img.SetNRGBA(1, 1, color.NRGBA{R: 40, G: 50, B: 60, A: 255})
	img.SetNRGBA(2, 1, color.NRGBA{R: 70, G: 80, B: 90, A: 255})

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode source png: %v", err)
	}
	dib, err := pngToDIB(buf.Bytes())
	if err != nil {
		t.Fatalf("pngToDIB: %v", err)
	}
	backPNG, err := dibToPNG(dib)
	if err != nil {
		t.Fatalf("dibToPNG: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(backPNG))
	if err != nil {
		t.Fatalf("decode roundtrip png: %v", err)
	}
	if decoded.Bounds().Dx() != 3 || decoded.Bounds().Dy() != 2 {
		t.Fatalf("unexpected bounds after roundtrip: %v", decoded.Bounds())
	}
}

// TestDIBToPNGZeroAlpha mimics what Paint / Snipping Tool put on the
// clipboard: a 32bpp BI_RGB DIB with alpha=0 across all pixels. Without the
// all-zero-alpha heuristic the image would decode as fully transparent and
// look blank in any peer's image viewer.
func TestDIBToPNGZeroAlpha(t *testing.T) {
	const w, h = 2, 2
	stride := w * 4
	hdr := make([]byte, 40)
	binary.LittleEndian.PutUint32(hdr[0:4], 40)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(w))
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(h))
	binary.LittleEndian.PutUint16(hdr[12:14], 1)
	binary.LittleEndian.PutUint16(hdr[14:16], 32)
	binary.LittleEndian.PutUint32(hdr[16:20], 0)
	pixels := make([]byte, stride*h)
	// BGRA with A=0 everywhere; distinct RGB so we can verify it survived.
	pixels[0], pixels[1], pixels[2], pixels[3] = 10, 20, 30, 0
	pixels[4], pixels[5], pixels[6], pixels[7] = 40, 50, 60, 0
	pixels[8], pixels[9], pixels[10], pixels[11] = 70, 80, 90, 0
	pixels[12], pixels[13], pixels[14], pixels[15] = 100, 110, 120, 0
	dib := append(hdr, pixels...)
	pngBytes, err := dibToPNG(dib)
	if err != nil {
		t.Fatalf("dibToPNG: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// At least one pixel must decode as opaque — otherwise the heuristic
	// didn't fire and the image came out fully transparent.
	anyOpaque := false
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a != 0 {
				anyOpaque = true
			}
		}
	}
	if !anyOpaque {
		t.Fatal("expected zero-alpha DIB to be treated as opaque after decode")
	}
}
