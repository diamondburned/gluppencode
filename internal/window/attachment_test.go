package window

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestDecodeBase64(t *testing.T) {
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 48, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 48; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 5), G: uint8(y * 5), B: 200, A: 255})
		}
	}
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	sampleB64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	t.Logf("valid sampleB64 = %s", sampleB64)

	tex, err := decodeBase64Texture(sampleB64)
	if err != nil {
		t.Fatalf("decodeBase64Texture failed: %v", err)
	}
	if tex == nil {
		t.Fatal("tex is nil")
	}
	t.Logf("width: %d, height: %d", tex.Width(), tex.Height())
}
