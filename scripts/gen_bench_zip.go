package main

import (
	"archive/zip"
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

func main() {
	var outPath string
	var imageCount int
	var width int
	var height int

	flag.StringVar(&outPath, "out", "", "output zip path")
	flag.IntVar(&imageCount, "images", 120, "number of PNG images")
	flag.IntVar(&width, "width", 256, "image width in pixels")
	flag.IntVar(&height, "height", 256, "image height in pixels")
	flag.Parse()

	if outPath == "" {
		exitf("-out is required")
	}
	if imageCount <= 0 {
		exitf("-images must be > 0")
	}
	if width <= 0 || height <= 0 {
		exitf("-width and -height must be > 0")
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		exitf("create output dir: %v", err)
	}

	f, err := os.Create(outPath)
	if err != nil {
		exitf("create zip file: %v", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	rng := rand.New(rand.NewSource(20260214))
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	modifiedAt := time.Unix(0, 0).UTC()

	for i := 0; i < imageCount; i++ {
		img := image.NewRGBA(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				img.SetRGBA(x, y, color.RGBA{
					R: uint8(rng.Intn(256)),
					G: uint8(rng.Intn(256)),
					B: uint8(rng.Intn(256)),
					A: 255,
				})
			}
		}

		var buf bytes.Buffer
		if err := enc.Encode(&buf, img); err != nil {
			exitf("encode image %d: %v", i, err)
		}

		hdr := &zip.FileHeader{
			Name:     fmt.Sprintf("%04d.png", i+1),
			Method:   zip.Store,
			Modified: modifiedAt,
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			exitf("create zip entry %d: %v", i, err)
		}
		if _, err := w.Write(buf.Bytes()); err != nil {
			exitf("write zip entry %d: %v", i, err)
		}
	}

	if err := zw.Close(); err != nil {
		exitf("close zip writer: %v", err)
	}
	if err := f.Close(); err != nil {
		exitf("close zip file: %v", err)
	}

	info, err := os.Stat(outPath)
	if err != nil {
		exitf("stat output zip: %v", err)
	}
	fmt.Printf("generated zip: %s (%d bytes, images=%d, %dx%d)\n", outPath, info.Size(), imageCount, width, height)
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
