//go:build windows || darwin

package tray

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

func TestEncodePNGIconDecodesToExpectedSize(t *testing.T) {
	data := encodePNGIcon()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png.Decode() = %v", err)
	}
	b := img.Bounds()
	if b.Dx() != iconSize || b.Dy() != iconSize {
		t.Errorf("decoded icon size = %dx%d, want %dx%d", b.Dx(), b.Dy(), iconSize, iconSize)
	}
}

func TestWrapICOHeaderAndEmbeddedPNG(t *testing.T) {
	png := encodePNGIcon()
	ico := wrapICO(png)

	if len(ico) != 6+16+len(png) {
		t.Fatalf("len(ico) = %d, want %d (header + entry + embedded PNG)", len(ico), 6+16+len(png))
	}
	if !bytes.Equal(ico[:6], []byte{0, 0, 1, 0, 1, 0}) {
		t.Errorf("ICONDIR header = % x, want reserved=0 type=1 count=1", ico[:6])
	}
	if ico[6] != iconSize || ico[7] != iconSize {
		t.Errorf("ICONDIRENTRY width/height = %d/%d, want %d/%d", ico[6], ico[7], iconSize, iconSize)
	}

	bytesInRes := binary.LittleEndian.Uint32(ico[14:18])
	offset := binary.LittleEndian.Uint32(ico[18:22])
	if int(bytesInRes) != len(png) {
		t.Errorf("bytesInRes = %d, want %d", bytesInRes, len(png))
	}
	if offset != 22 {
		t.Errorf("image data offset = %d, want 22", offset)
	}
	if !bytes.Equal(ico[offset:], png) {
		t.Error("embedded image data doesn't match the original PNG bytes")
	}
}
