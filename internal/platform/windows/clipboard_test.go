//go:build windows

package windows

import (
	"bytes"
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
