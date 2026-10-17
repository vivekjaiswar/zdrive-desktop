//go:build windows || darwin

package tray

import (
	"bytes"
	_ "embed"
	"runtime"
)

const iconSize = 64

//go:embed icon.png
var iconPNG []byte

// iconBytes returns the tray icon - the real ZDrive mark (cloud+Z),
// cropped and squared from the brand logo, embedded at build time
// rather than drawn at runtime.
//
// The two backends decode bytes differently (confirmed by reading their
// native source, not assumed): Windows just writes the bytes to a .ico
// file and hands it to the Win32 icon loader (which supports a
// PNG-compressed single-image ICO container since Vista); macOS passes
// the bytes straight to NSImage's initWithData:, which decodes PNG/JPEG
// directly but has no idea what an .ico container is. Same source image,
// wrapped differently per platform.
func iconBytes() []byte {
	if runtime.GOOS == "windows" {
		return wrapICO(encodePNGIcon())
	}
	return encodePNGIcon()
}

func encodePNGIcon() []byte {
	return iconPNG
}

// wrapICO builds the minimal single-image ICO container (a 6-byte
// ICONDIR plus one 16-byte ICONDIRENTRY) around an already-PNG-encoded
// image. Windows Vista+ accepts a PNG-compressed image directly inside
// an .ico file, which is far simpler than hand-encoding a raw BMP
// bitmap - this only needs the container framing, not a second image
// codec.
func wrapICO(pngData []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0, 0, 1, 0, 1, 0}) // ICONDIR: reserved=0, type=1 (icon), count=1
	b.WriteByte(iconSize)             // width (0 would mean 256)
	b.WriteByte(iconSize)             // height
	b.WriteByte(0)                    // color count (0 = not a palette image)
	b.WriteByte(0)                    // reserved
	b.Write([]byte{1, 0})             // planes
	b.Write([]byte{32, 0})            // bits per pixel
	writeUint32LE(&b, uint32(len(pngData)))
	writeUint32LE(&b, 6+16) // image data offset: ICONDIR + one ICONDIRENTRY
	b.Write(pngData)
	return b.Bytes()
}

func writeUint32LE(b *bytes.Buffer, v uint32) {
	b.WriteByte(byte(v))
	b.WriteByte(byte(v >> 8))
	b.WriteByte(byte(v >> 16))
	b.WriteByte(byte(v >> 24))
}
