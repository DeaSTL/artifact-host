package preview

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

const (
	viewportWidth  = 1280
	viewportHeight = 800
	thumbWidth     = 640
	thumbHeight    = 400
)

// downscaleImage resizes src to dstW x dstH using box/area averaging.
// When src is an exact 2x multiple of the target size (such as 1280x800 -> 640x400),
// a fast 2x2 averaging path is used.
func downscaleImage(src image.Image, dstW, dstH int) *image.RGBA {
	if dstW <= 0 || dstH <= 0 {
		return image.NewRGBA(image.Rect(0, 0, max(dstW, 0), max(dstH, 0)))
	}

	bounds := src.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	if srcW <= 0 || srcH <= 0 {
		return dst
	}

	if srcW == dstW*2 && srcH == dstH*2 {
		switch img := src.(type) {
		case *image.RGBA:
			downscale2xRGBA(img, dst)
			return dst
		case *image.NRGBA:
			downscale2xNRGBA(img, dst)
			return dst
		default:
			downscale2xGeneric(src, dst)
			return dst
		}
	}

	downscaleAreaAverage(src, dst)
	return dst
}

func downscale2xRGBA(src *image.RGBA, dst *image.RGBA) {
	sb := src.Bounds()
	dstW := dst.Bounds().Dx()
	dstH := dst.Bounds().Dy()

	for dy := 0; dy < dstH; dy++ {
		sy := sb.Min.Y + dy*2
		row0 := src.PixOffset(sb.Min.X, sy)
		row1 := src.PixOffset(sb.Min.X, sy+1)
		dstRow := dst.PixOffset(0, dy)

		for dx := 0; dx < dstW; dx++ {
			i00 := row0 + dx*8
			i01 := i00 + 4
			i10 := row1 + dx*8
			i11 := i10 + 4
			di := dstRow + dx*4

			r := uint32(src.Pix[i00]) + uint32(src.Pix[i01]) + uint32(src.Pix[i10]) + uint32(src.Pix[i11])
			g := uint32(src.Pix[i00+1]) + uint32(src.Pix[i01+1]) + uint32(src.Pix[i10+1]) + uint32(src.Pix[i11+1])
			b := uint32(src.Pix[i00+2]) + uint32(src.Pix[i01+2]) + uint32(src.Pix[i10+2]) + uint32(src.Pix[i11+2])
			a := uint32(src.Pix[i00+3]) + uint32(src.Pix[i01+3]) + uint32(src.Pix[i10+3]) + uint32(src.Pix[i11+3])

			dst.Pix[di] = uint8((r + 2) >> 2)
			dst.Pix[di+1] = uint8((g + 2) >> 2)
			dst.Pix[di+2] = uint8((b + 2) >> 2)
			dst.Pix[di+3] = uint8((a + 2) >> 2)
		}
	}
}

func downscale2xNRGBA(src *image.NRGBA, dst *image.RGBA) {
	sb := src.Bounds()
	dstW := dst.Bounds().Dx()
	dstH := dst.Bounds().Dy()

	for dy := 0; dy < dstH; dy++ {
		sy := sb.Min.Y + dy*2
		row0 := src.PixOffset(sb.Min.X, sy)
		row1 := src.PixOffset(sb.Min.X, sy+1)
		dstRow := dst.PixOffset(0, dy)

		for dx := 0; dx < dstW; dx++ {
			i00 := row0 + dx*8
			i01 := i00 + 4
			i10 := row1 + dx*8
			i11 := i10 + 4
			di := dstRow + dx*4

			a00 := uint32(src.Pix[i00+3])
			a01 := uint32(src.Pix[i01+3])
			a10 := uint32(src.Pix[i10+3])
			a11 := uint32(src.Pix[i11+3])

			if a00 == 255 && a01 == 255 && a10 == 255 && a11 == 255 {
				r := uint32(src.Pix[i00]) + uint32(src.Pix[i01]) + uint32(src.Pix[i10]) + uint32(src.Pix[i11])
				g := uint32(src.Pix[i00+1]) + uint32(src.Pix[i01+1]) + uint32(src.Pix[i10+1]) + uint32(src.Pix[i11+1])
				b := uint32(src.Pix[i00+2]) + uint32(src.Pix[i01+2]) + uint32(src.Pix[i10+2]) + uint32(src.Pix[i11+2])
				dst.Pix[di] = uint8((r + 2) >> 2)
				dst.Pix[di+1] = uint8((g + 2) >> 2)
				dst.Pix[di+2] = uint8((b + 2) >> 2)
				dst.Pix[di+3] = 255
				continue
			}

			// Premultiply each NRGBA sample before averaging into RGBA.
			r := uint32(src.Pix[i00])*a00 + uint32(src.Pix[i01])*a01 + uint32(src.Pix[i10])*a10 + uint32(src.Pix[i11])*a11
			g := uint32(src.Pix[i00+1])*a00 + uint32(src.Pix[i01+1])*a01 + uint32(src.Pix[i10+1])*a10 + uint32(src.Pix[i11+1])*a11
			b := uint32(src.Pix[i00+2])*a00 + uint32(src.Pix[i01+2])*a01 + uint32(src.Pix[i10+2])*a10 + uint32(src.Pix[i11+2])*a11
			a := a00 + a01 + a10 + a11

			const denom = 4 * 255
			dst.Pix[di] = uint8((r + denom/2) / denom)
			dst.Pix[di+1] = uint8((g + denom/2) / denom)
			dst.Pix[di+2] = uint8((b + denom/2) / denom)
			dst.Pix[di+3] = uint8((a + 2) >> 2)
		}
	}
}

func downscale2xGeneric(src image.Image, dst *image.RGBA) {
	sb := src.Bounds()
	dstW := dst.Bounds().Dx()
	dstH := dst.Bounds().Dy()

	for dy := 0; dy < dstH; dy++ {
		sy := sb.Min.Y + dy*2
		for dx := 0; dx < dstW; dx++ {
			sx := sb.Min.X + dx*2
			r0, g0, b0, a0 := src.At(sx, sy).RGBA()
			r1, g1, b1, a1 := src.At(sx+1, sy).RGBA()
			r2, g2, b2, a2 := src.At(sx, sy+1).RGBA()
			r3, g3, b3, a3 := src.At(sx+1, sy+1).RGBA()

			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8(((r0 + r1 + r2 + r3 + 2) >> 2) >> 8),
				G: uint8(((g0 + g1 + g2 + g3 + 2) >> 2) >> 8),
				B: uint8(((b0 + b1 + b2 + b3 + 2) >> 2) >> 8),
				A: uint8(((a0 + a1 + a2 + a3 + 2) >> 2) >> 8),
			})
		}
	}
}

func downscaleAreaAverage(src image.Image, dst *image.RGBA) {
	sb := src.Bounds()
	srcW := sb.Dx()
	srcH := sb.Dy()
	dstW := dst.Bounds().Dx()
	dstH := dst.Bounds().Dy()

	scaleX := float64(srcW) / float64(dstW)
	scaleY := float64(srcH) / float64(dstH)

	for dy := 0; dy < dstH; dy++ {
		sy0 := float64(dy) * scaleY
		sy1 := float64(dy+1) * scaleY
		yStart := int(math.Floor(sy0))
		yEnd := int(math.Ceil(sy1))
		if yEnd > srcH {
			yEnd = srcH
		}

		for dx := 0; dx < dstW; dx++ {
			sx0 := float64(dx) * scaleX
			sx1 := float64(dx+1) * scaleX
			xStart := int(math.Floor(sx0))
			xEnd := int(math.Ceil(sx1))
			if xEnd > srcW {
				xEnd = srcW
			}

			var sumR, sumG, sumB, sumA, totalW float64
			for sy := yStart; sy < yEnd; sy++ {
				wy := math.Min(float64(sy+1), sy1) - math.Max(float64(sy), sy0)
				if wy <= 0 {
					continue
				}
				for sx := xStart; sx < xEnd; sx++ {
					wx := math.Min(float64(sx+1), sx1) - math.Max(float64(sx), sx0)
					if wx <= 0 {
						continue
					}
					w := wx * wy
					r, g, b, a := src.At(sb.Min.X+sx, sb.Min.Y+sy).RGBA()
					sumR += float64(r) * w
					sumG += float64(g) * w
					sumB += float64(b) * w
					sumA += float64(a) * w
					totalW += w
				}
			}

			if totalW <= 0 {
				continue
			}
			inv := 1.0 / (totalW * 257.0)
			dst.SetRGBA(dx, dy, color.RGBA{
				R: clampUint8(sumR * inv),
				G: clampUint8(sumG * inv),
				B: clampUint8(sumB * inv),
				A: clampUint8(sumA * inv),
			})
		}
	}
}

func clampUint8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(math.Round(v))
}

func downscaleAndWritePNG(srcPath, dstPath string, dstW, dstH int) error {
	inFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open raw screenshot: %w", err)
	}
	defer inFile.Close()

	srcImg, err := png.Decode(inFile)
	if err != nil {
		return fmt.Errorf("decode raw screenshot png: %w", err)
	}

	thumb := downscaleImage(srcImg, dstW, dstH)

	dstDir := filepath.Dir(dstPath)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("create preview directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(dstDir, ".preview-*.png.tmp")
	if err != nil {
		return fmt.Errorf("create temp preview file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := encoder.Encode(tmpFile, thumb); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("encode thumbnail png: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp preview file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("chmod temp preview file: %w", err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename preview file: %w", err)
	}
	cleanup = false
	return nil
}
