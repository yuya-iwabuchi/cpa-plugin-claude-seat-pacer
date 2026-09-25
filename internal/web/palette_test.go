package web

import (
	"math"
	"regexp"
	"strconv"
	"testing"
)

// TestSeatHuesHoldTheirBounds covers every theme the page declares seat hues
// in: each hue clears 3:1 against that theme's card, and the first four, which
// a small pool draws, stay at least ΔE 15 apart in OKLab (×100), the distance
// at which two thin lines read as two series.
func TestSeatHuesHoldTheirBounds(t *testing.T) {
	t.Parallel()
	page := string(indexHTML)
	blocks := regexp.MustCompile(`\{[^{}]*--seat-1:[^{}]*\}`).FindAllString(page, -1)
	if len(blocks) != 3 {
		t.Fatalf("found %d token blocks declaring seat hues, want the light one and the two dark ones", len(blocks))
	}
	decl := regexp.MustCompile(`--(surface-1|seat-(\d+)):\s*(#[0-9a-f]{6});`)
	for bi, b := range blocks {
		var card string
		seats := map[int]string{}
		for _, m := range decl.FindAllStringSubmatch(b, -1) {
			if m[1] == "surface-1" {
				card = m[3]
				continue
			}
			n, _ := strconv.Atoi(m[2])
			seats[n] = m[3]
		}
		if card == "" || len(seats) != 12 {
			t.Fatalf("block %d: card %q and %d seat hues, want a card and 12 hues", bi, card, len(seats))
		}
		for n := 1; n <= 12; n++ {
			if r := contrastRatio(seats[n], card); r < 3 {
				t.Errorf("block %d: --seat-%d %s is %.2f:1 on the card %s, want at least 3:1", bi, n, seats[n], r, card)
			}
		}
		for i := 1; i <= 4; i++ {
			for j := i + 1; j <= 4; j++ {
				if d := oklabDistance(seats[i], seats[j]); d < 15 {
					t.Errorf("block %d: --seat-%d and --seat-%d are ΔE %.1f apart, want at least 15", bi, i, j, d)
				}
			}
		}
	}
}

func linearRGB(hex string) [3]float64 {
	var c [3]float64
	for i := range c {
		v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		s := float64(v) / 255
		if s <= 0.04045 {
			c[i] = s / 12.92
		} else {
			c[i] = math.Pow((s+0.055)/1.055, 2.4)
		}
	}
	return c
}

func contrastRatio(a, b string) float64 {
	lum := func(h string) float64 {
		c := linearRGB(h)
		return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
	}
	hi, lo := lum(a), lum(b)
	if lo > hi {
		hi, lo = lo, hi
	}
	return (hi + 0.05) / (lo + 0.05)
}

func oklabDistance(a, b string) float64 {
	lab := func(h string) [3]float64 {
		c := linearRGB(h)
		l := math.Cbrt(0.4122214708*c[0] + 0.5363325363*c[1] + 0.0514459929*c[2])
		m := math.Cbrt(0.2119034982*c[0] + 0.6806995451*c[1] + 0.1073969566*c[2])
		s := math.Cbrt(0.0883024619*c[0] + 0.2817188376*c[1] + 0.6299787005*c[2])
		return [3]float64{
			0.2104542553*l + 0.7936177850*m - 0.0040720468*s,
			1.9779984951*l - 2.4285922050*m + 0.4505937099*s,
			0.0259040371*l + 0.7827717662*m - 0.8086757660*s,
		}
	}
	x, y := lab(a), lab(b)
	return 100 * math.Sqrt((x[0]-y[0])*(x[0]-y[0])+(x[1]-y[1])*(x[1]-y[1])+(x[2]-y[2])*(x[2]-y[2]))
}
