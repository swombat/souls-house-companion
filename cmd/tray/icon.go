//go:build linux

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// The house mark, as the Mac menu draws it (menubar/Sources/SoulsHouseMenu):
// a roof chevron over a coral circle, on the logo's 240 x 210 grid. Filled
// circle: the companion is running. Coral ring: paused. Grey ring: the
// companion is not answering.
type mark int

const (
	markRunning mark = iota
	markPaused
	markUnknown
)

var (
	houseCoral = color.NRGBA{0xf1, 0x5d, 0x61, 0xff}
	// StatusNotifierItem has no "label colour" to follow, so the roof is a light
	// neutral that reads on Omarchy's dark bars and still shows on a light one.
	roofColour  = color.NRGBA{0xe8, 0xe6, 0xe3, 0xff}
	unknownGrey = color.NRGBA{0x8a, 0x8a, 0x8a, 0xff}
)

const iconSize = 64

// renderMark draws the mark into a square iconSize PNG, 4x4 supersampled.
func renderMark(m mark) []byte {
	const ss = 4
	// Fit the 240 x 210 logo into the square with a small margin.
	scale := float64(iconSize) * 0.92 / 240.0
	offX := (float64(iconSize) - 240*scale) / 2
	offY := (float64(iconSize) - 210*scale) / 2
	pt := func(x, y float64) (float64, float64) { return offX + x*scale, offY + y*scale }

	ax, ay := pt(16, 100)
	bx, by := pt(120, 24)
	cx, cy := pt(224, 100)
	roofHalf := 26 * scale / 2
	ox, oy := pt(120, 148)
	orbR := 58 * scale
	ringW := 22 * scale

	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	for py := 0; py < iconSize; py++ {
		for px := 0; px < iconSize; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := float64(px) + (float64(sx)+0.5)/ss
					y := float64(py) + (float64(sy)+0.5)/ss
					c, ok := sample(m, x, y, ax, ay, bx, by, cx, cy, roofHalf, ox, oy, orbR, ringW)
					if !ok {
						continue
					}
					r += float64(c.R)
					g += float64(c.G)
					b += float64(c.B)
					a += 255
				}
			}
			n := float64(ss * ss)
			if a == 0 {
				continue
			}
			covered := a / 255
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(r / covered), G: uint8(g / covered), B: uint8(b / covered), A: uint8(a / n),
			})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func sample(m mark, x, y, ax, ay, bx, by, cx, cy, roofHalf, ox, oy, orbR, ringW float64) (color.NRGBA, bool) {
	if segDist(x, y, ax, ay, bx, by) <= roofHalf || segDist(x, y, bx, by, cx, cy) <= roofHalf {
		return roofColour, true
	}
	d := math.Hypot(x-ox, y-oy)
	switch m {
	case markRunning:
		if d <= orbR {
			return houseCoral, true
		}
	default:
		// Keep the ring inside the filled orb's outline so the mark doesn't grow.
		mid := orbR - ringW/2
		if math.Abs(d-mid) <= ringW/2 {
			if m == markPaused {
				return houseCoral, true
			}
			return unknownGrey, true
		}
	}
	return color.NRGBA{}, false
}

// segDist is the distance from (x, y) to the segment a-b (round caps).
func segDist(x, y, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := ((x-ax)*dx + (y-ay)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(x-(ax+t*dx), y-(ay+t*dy))
}
