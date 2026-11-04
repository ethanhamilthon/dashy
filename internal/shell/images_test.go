package shell

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeImage(t *testing.T, src image.Image, format string) (string, []byte) {
	t.Helper()
	var out bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&out, src, &jpeg.Options{Quality: 100})
	} else {
		err = png.Encode(&out, src)
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source."+format)
	if err = os.WriteFile(path, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path, out.Bytes()
}

func decodePreview(t *testing.T, img Image) (image.Image, string, []byte) {
	t.Helper()
	prefix, encoded, ok := strings.Cut(img.URL, ";base64,")
	if !ok {
		t.Fatal("missing image data URL")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	preview, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if prefix != "data:image/"+format || len(data) > maxImageBytes || max(preview.Bounds().Dx(), preview.Bounds().Dy()) > maxImageEdge {
		t.Fatalf("unbounded/mislabeled preview: %s, %d bytes, %v", prefix, len(data), preview.Bounds())
	}
	return preview, format, data
}

func TestImagePreviewsResizeAndPreserveOriginals(t *testing.T) {
	for _, tc := range []struct {
		name, format                         string
		width, height, wantWidth, wantHeight int
	}{
		{"wide-sheet-regression", "jpeg", 3290, 1170, 2048, 728},
		{"portrait", "png", 1170, 3290, 728, 2048},
		{"small", "png", 25, 15, 25, 15},
		{"thin", "png", 1, 4000, 1, 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := image.NewNRGBA(image.Rect(0, 0, tc.width, tc.height))
			src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 100})
			path, original := writeImage(t, src, tc.format)
			directory := filepath.Join(t.TempDir(), "originals")
			captured, err := CaptureImageCopy(path, directory)
			if err != nil {
				t.Fatal(err)
			}
			preview, _, data := decodePreview(t, captured)
			if preview.Bounds().Dx() != tc.wantWidth || preview.Bounds().Dy() != tc.wantHeight {
				t.Fatalf("wrong dimensions: %v", preview.Bounds())
			}
			if tc.format == "png" {
				_, _, _, alpha := preview.At(preview.Bounds().Max.X-1, preview.Bounds().Max.Y-1).RGBA()
				if alpha != 0 {
					t.Fatal("lost transparency")
				}
			}
			if tc.name == "small" && !bytes.Equal(data, original) {
				t.Fatal("unnecessarily re-encoded small image")
			}
			for _, p := range []string{path, captured.Path} {
				got, err := os.ReadFile(p)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatal("original not preserved byte-for-byte", err)
				}
			}
			info, err := os.Stat(captured.Path)
			if err != nil || info.Mode().Perm() != 0600 || filepath.Dir(captured.Path) != directory {
				t.Fatal("original not stored privately", err)
			}
			if err = os.WriteFile(path, []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(captured.Path)
			if !bytes.Equal(got, original) {
				t.Fatal("snapshot changed with source")
			}
		})
	}
}

func TestImagePreviewsBoundNoisyFiles(t *testing.T) {
	for _, transparent := range []bool{false, true} {
		t.Run(map[bool]string{false: "opaque", true: "transparent"}[transparent], func(t *testing.T) {
			src := image.NewNRGBA(image.Rect(0, 0, 1100, 1100))
			rand.New(rand.NewSource(42)).Read(src.Pix)
			if !transparent {
				for i := 3; i < len(src.Pix); i += 4 {
					src.Pix[i] = 255
				}
			}
			path, original := writeImage(t, src, "png")
			if len(original) <= 2*1024*1024 {
				t.Fatal("fixture must exceed old 2 MiB input limit")
			}
			captured, err := CaptureImageCopy(path, "")
			if err != nil {
				t.Fatal(err)
			}
			preview, format, _ := decodePreview(t, captured)
			if transparent && format != "png" {
				t.Fatal("transparent image became JPEG")
			}
			if !transparent && format != "jpeg" {
				t.Fatal("opaque noisy PNG should fit as JPEG")
			}
			if transparent && preview.Bounds().Dx() >= src.Bounds().Dx() {
				t.Fatal("noisy transparent image should shrink to fit byte budget")
			}
		})
	}
}

func TestViewRejectsInvalidAndOversizedImages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.png")
	_, pngData := writeImage(t, image.NewNRGBA(image.Rect(0, 0, 2, 2)), "png")
	oversized := bytes.Clone(pngData)
	binary.BigEndian.PutUint32(oversized[16:20], 5000)
	binary.BigEndian.PutUint32(oversized[20:24], 5000)
	binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
	for _, data := range [][]byte{[]byte("not an image"), pngData[:33], oversized} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := CaptureImageCopy(path, ""); err == nil {
			t.Fatal("accepted invalid/truncated/oversized image")
		}
	}
	if err := os.Truncate(path, MaxImageInputBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureImageCopy(path, ""); err == nil || !strings.Contains(err.Error(), "32 MiB") {
		t.Fatal("accepted oversized input", err)
	}
	if _, err := CaptureImageCopy(dir, ""); err == nil {
		t.Fatal("accepted directory")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("invalid image left an original snapshot")
	}
}
