package value

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif" // register decoders for image.DecodeConfig
	_ "image/jpeg"
	_ "image/png"
	"io"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// imageFormats are the formats an image value may hold (spec/types.md §5).
var imageFormats = map[string]bool{"png": true, "jpeg": true, "gif": true, "webp": true, "bmp": true, "tiff": true}

// Image is an encoded image file with its format and dimensions read from
// its header. The original bytes are kept unchanged.
type Image struct {
	format        string
	width, height int
	data          []byte
}

// NewImage checks that data is a png, jpeg, gif, webp, bmp or tiff file and
// returns it as an Image holding a copy of data. Only the header is decoded.
func NewImage(data []byte) (Image, error) {
	return newImage(bytes.Clone(data))
}

// newImage is NewImage without the copy.
func newImage(data []byte) (Image, error) {
	if len(data) > MaxBytesLen {
		return Image{}, fmt.Errorf("image longer than %d bytes", MaxBytesLen)
	}
	c, err := imageConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, err
	}
	return Image{c.format, c.width, c.height, data}, nil
}

type imageInfo struct {
	format        string
	width, height int
}

// imageConfig reads an image's header.
func imageConfig(r io.Reader) (imageInfo, error) {
	cfg, format, err := image.DecodeConfig(r)
	if err != nil {
		return imageInfo{}, fmt.Errorf("not a supported image (png, jpeg, gif, webp, bmp or tiff): %v", err)
	}
	if !imageFormats[format] {
		return imageInfo{}, fmt.Errorf("unsupported image format %q", format)
	}
	return imageInfo{format, cfg.Width, cfg.Height}, nil
}

func (Image) Code() Code { return CodeImage }

// Format returns "png", "jpeg", "gif", "webp", "bmp" or "tiff".
func (i Image) Format() string { return i.format }
func (i Image) Width() int     { return i.width }
func (i Image) Height() int    { return i.height }

// Data returns a copy of the encoded file.
func (i Image) Data() []byte { return bytes.Clone(i.data) }

// Len returns the size of the encoded file in bytes.
func (i Image) Len() int { return len(i.data) }

// String returns a cast expression that rebuilds the image from its bytes.
func (i Image) String() string {
	return `cast(x"` + hex.EncodeToString(i.data) + `" as image)`
}
