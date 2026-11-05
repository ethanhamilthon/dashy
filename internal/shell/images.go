package shell

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/image/draw"
)

const (
	MaxImageInputBytes = 32 * 1024 * 1024
	maxImageBytes      = 1024 * 1024
	maxImageEdge       = 2048
)

type Image struct {
	Path string `json:"path,omitempty"`
	URL  string `json:"url"`
}

// CaptureImageCopy embeds a bounded preview and saves an immutable original in directory
// for later crops (none when directory is empty), leaving the file at path untouched.
func CaptureImageCopy(path, directory string) (Image, error) {
	// Check the file type before os.Open, which blocks on a FIFO until a writer
	// appears. Nonblocking open also handles a replacement between stat and open.
	if info, err := os.Stat(path); err != nil {
		return Image{}, err
	} else if !info.Mode().IsRegular() {
		return Image{}, fmt.Errorf("view requires a regular image file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Image{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Image{}, err
	}
	if !info.Mode().IsRegular() {
		return Image{}, fmt.Errorf("view requires a regular image file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxImageInputBytes+1))
	if err != nil {
		return Image{}, err
	}
	if path, err = filepath.Abs(path); err != nil {
		return Image{}, err
	}
	return CaptureImageData(data, path, directory)
}

// CaptureImageData is CaptureImageCopy for image bytes; path names them when directory is empty.
func CaptureImageData(data []byte, path, directory string) (Image, error) {
	if len(data) > MaxImageInputBytes {
		return Image{}, fmt.Errorf("view image exceeds %d MiB", MaxImageInputBytes>>20)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") {
		return Image{}, fmt.Errorf("view requires a valid PNG or JPEG image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 20_000_000 {
		return Image{}, fmt.Errorf("view image exceeds 20 megapixels")
	}
	preview, mime, err := imagePreview(data, cfg, format)
	if err != nil {
		return Image{}, err
	}
	if directory != "" {
		if err = os.MkdirAll(directory, 0700); err != nil {
			return Image{}, err
		}
		original, err := os.CreateTemp(directory, "original-*."+format)
		if err != nil {
			return Image{}, err
		}
		_, writeErr := original.Write(data)
		closeErr := original.Close()
		if writeErr != nil || closeErr != nil {
			os.Remove(original.Name())
			return Image{}, fmt.Errorf("save original: %w", errors.Join(writeErr, closeErr))
		}
		path, err = filepath.Abs(original.Name())
		if err != nil {
			return Image{}, err
		}
	}
	return Image{Path: path, URL: "data:image/" + mime + ";base64," + base64.StdEncoding.EncodeToString(preview)}, nil
}

func imagePreview(data []byte, cfg image.Config, format string) ([]byte, string, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}
	if max(cfg.Width, cfg.Height) <= maxImageEdge && len(data) <= maxImageBytes {
		return data, format, nil
	}
	edge := min(maxImageEdge, max(cfg.Width, cfg.Height))
	for {
		width := max(1, cfg.Width*edge/max(cfg.Width, cfg.Height))
		height := max(1, cfg.Height*edge/max(cfg.Width, cfg.Height))
		dst := image.NewNRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
		var out bytes.Buffer
		if format == "jpeg" {
			err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85})
		} else {
			err = png.Encode(&out, dst)
			if err == nil && out.Len() > maxImageBytes && dst.Opaque() {
				format = "jpeg"
				out.Reset()
				err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85})
			}
		}
		if err != nil {
			return nil, "", fmt.Errorf("encode preview: %w", err)
		}
		if out.Len() <= maxImageBytes {
			return out.Bytes(), format, nil
		}
		edge = max(1, edge*3/4)
	}
}
