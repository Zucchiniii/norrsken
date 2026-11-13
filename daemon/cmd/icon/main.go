// Command icon renders the Norrsken app icons (aurora over a night sky with
// a shield outline) as PNGs. Run: go run ./tools/icon <outdir>
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

type rgb struct{ r, g, b float64 }

func mix(a, b rgb, t float64) rgb {
	return rgb{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t}
}
func clamp(x float64) float64 { return math.Max(0, math.Min(1, x)) }
func smooth(e0, e1, x float64) float64 {
	t := clamp((x - e0) / (e1 - e0))
	return t * t * (3 - 2*t)
}

// sdRoundBox is the signed distance to a rounded square centred at 0.
func sdRoundBox(x, y, half, r float64) float64 {
	qx, qy := math.Abs(x)-half+r, math.Abs(y)-half+r
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
}

// shield outline distance (approximate SDF built from a box top + circle-ish bottom).
func shieldDist(x, y float64) float64 {
	// Shield: top edge at y=-0.30, sides to y=0.02, curving to a point at y=0.36.
	w := 0.25
	var d float64
	if y < 0.02 {
		top := -0.31 + 0.055*(math.Abs(x)/w)
		d = math.Max(math.Abs(x)-w, top-y)
	} else {
		// lower part narrows towards the tip
		// half-width shrinks as w*(1-t^2): straight sides that meet in a point.
		t := (y - 0.02) / 0.34
		if t >= 1 {
			d = math.Hypot(x, y-0.36)
		} else {
			half := w * (1 - t*t)
			slope := w * 2 * t / 0.34
			d = (math.Abs(x) - half) / math.Sqrt(1+slope*slope)
		}
	}
	return d
}

func pixel(u, v float64) (rgb, float64) {
	x, y := u-0.5, v-0.5
	alpha := 1 - smooth(-0.004, 0.004, sdRoundBox(x, y, 0.5, 0.2))
	night := mix(rgb{0.03, 0.045, 0.09}, rgb{0.012, 0.02, 0.045}, clamp(v*1.1))
	col := night
	// aurora curtains: wavy band near the top with vertical streaks
	wave := 0.27 + 0.055*math.Sin(u*5.2+0.4) + 0.012*math.Sin(u*12+1.3)
	band := math.Exp(-math.Pow((v-wave)/0.075, 2))
	streak := 0.72 + 0.28*math.Pow(math.Abs(math.Sin(u*21+2.2*math.Sin(u*6))), 2)
	tail := math.Exp(-math.Max(0, v-wave)/0.24) * smooth(wave-0.10, wave+0.02, v)
	glow := clamp(band*streak*1.15 + tail*0.55*streak)
	green, teal, violet := rgb{0.31, 0.94, 0.70}, rgb{0.21, 0.84, 0.91}, rgb{0.60, 0.48, 1.0}
	hueT := clamp(u*1.15 - 0.05)
	ac := mix(mix(green, teal, smooth(0, 0.55, hueT)), violet, smooth(0.55, 1, hueT))
	ac = mix(ac, violet, smooth(wave-0.02, wave-0.2, v)*0.6)
	col = mix(col, ac, glow*0.92)
	// stars
	for _, s := range [][3]float64{{0.18, 0.62, 1}, {0.76, 0.70, 0.8}, {0.30, 0.82, 0.7}, {0.86, 0.48, 0.9}, {0.62, 0.86, 0.6}} {
		d := math.Hypot(u-s[0], v-s[1])
		col = mix(col, rgb{1, 1, 1}, s[2]*math.Exp(-d*d/0.00003))
	}
	// shield outline, slightly below centre
	sd := shieldDist(x*1.28, (y-0.05)*1.28) / 1.28
	ring := 1 - smooth(0.012, 0.022, math.Abs(sd))
	col = mix(col, rgb{0.95, 0.98, 1}, ring*0.95)
	inside := 1 - smooth(-0.01, 0.0, sd)
	col = mix(col, mix(col, rgb{0.02, 0.03, 0.07}, 0.55), inside*(1-ring))
	return col, alpha
}

func render(size int, path string) {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var acc rgb
			var a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					u := (float64(px) + (float64(sx)+0.5)/ss) / float64(size)
					v := (float64(py) + (float64(sy)+0.5)/ss) / float64(size)
					c, al := pixel(u, v)
					acc.r += c.r * al
					acc.g += c.g * al
					acc.b += c.b * al
					a += al
				}
			}
			n := float64(ss * ss)
			if a > 0 {
				acc = rgb{acc.r / a, acc.g / a, acc.b / a}
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(clamp(acc.r) * 255), uint8(clamp(acc.g) * 255), uint8(clamp(acc.b) * 255), uint8(clamp(a/n) * 255)})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	png.Encode(f, img)
}

func main() {
	out := "."
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	render(80, filepath.Join(out, "icon.png"))
	render(130, filepath.Join(out, "largeIcon.png"))
	render(512, filepath.Join(out, "icon-512.png"))
}
