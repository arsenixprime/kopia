//go:build !no_extra_providers

package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"strings"
)

// A hand-rolled SVG canvas.
//
// The charts are written by hand rather than with a plotting library because
// the harness's dependency set is frozen, and because the output has a specific
// job: it is pasted into a GitHub pull-request body, where it is rendered as an
// image with no scripting, no external stylesheet and no font loading.
//
// Two consequences shape everything below:
//
//   - Every style is inline, in one <style> element, and every color is baked.
//     GitHub renders an SVG in a context where prefers-color-scheme cannot be
//     relied on, so the page CANNOT adapt to the reader's theme. It therefore
//     carries its own light surface as a filled rectangle behind everything, and
//     is drawn in dark ink on it - which is legible in both GitHub themes,
//     unlike a transparent background, which turns black text invisible in dark
//     mode.
//   - Nothing can be measured. There is no text metric available at generation
//     time, so every label is placed against approxTextWidth and every column
//     that holds text is sized from it. Labels are placed OUTSIDE the marks for
//     the same reason: a label that does not fit must never be clipped.

// The palette. Categorical slots 1 and 2 of the reference data-visualization
// palette, on its light surface; the pair validates on every check (lightness
// band, chroma floor, CVD separation ΔE 24.7, normal-vision ΔE 33.6, contrast
// >= 3:1 against the surface) with all pairs in play.
const (
	colorSurface  = "#fcfcfb"
	colorInk      = "#0b0b0b"
	colorInk2     = "#52514e"
	colorInk3     = "#7a7975"
	colorGrid     = "#e6e5e1"
	colorAxis     = "#c9c8c3"
	colorSeries1  = "#2a78d6" // blue
	colorSeries2  = "#eb6834" // orange
	colorCritical = "#d03b3b" // status: reserved, used only for BROKEN-ACCOUNTING
)

// Type sizes and mark specs.
const (
	fontTitle    = 16.0
	fontSubtitle = 11.5
	fontLabel    = 11.0
	fontValue    = 10.5
	fontCaption  = 10.0

	barThickness = 14.0 // <= 24px, per the mark spec.
	barGap       = 2.0  // the surface gap between two touching bars.
	barRadius    = 4.0  // rounded data-end, square at the baseline.
	whiskerCap   = 5.0

	// approxCharWidth is the average advance of the sans stack at 1em. It is an
	// estimate, so every column sized with it gets padding on top.
	approxCharWidth = 0.55

	// minVisibleBarPx is the width a bar is drawn at when its value scales to
	// less than that: a lane that is 100x faster must still be a visible mark and
	// not an absence.
	minVisibleBarPx = 0.5

	// decadeBase is the base of the tick ladder, and msPerSecond / secondsCutoffMS
	// are where formatMS switches from milliseconds to seconds.
	decadeBase      = 10.0
	msPerSecond     = 1000.0
	secondsCutoffMS = 10000.0
)

// svgCanvas accumulates SVG elements.
type svgCanvas struct {
	w, h float64
	body bytes.Buffer
}

func newSVGCanvas(w, h float64) *svgCanvas {
	return &svgCanvas{w: w, h: h}
}

// approxTextWidth estimates how wide a string will render.
func approxTextWidth(s string, size float64) float64 {
	return float64(len([]rune(s))) * size * approxCharWidth
}

func (c *svgCanvas) rect(x, y, w, h float64, fill, extra string) {
	if w <= 0 || h <= 0 {
		return
	}

	fmt.Fprintf(&c.body, "<rect x=%q y=%q width=%q height=%q fill=%q%v/>\n",
		num(x), num(y), num(w), num(h), fill, extra)
}

// bar draws a horizontal bar with a rounded data-end and a square baseline end.
func (c *svgCanvas) bar(x, y, w, h float64, fill, extra string) {
	if h <= 0 {
		return
	}

	r := math.Min(barRadius, math.Min(w/halfDivisor, h/halfDivisor))

	if w <= minVisibleBarPx {
		// A bar too short to round still has to be visible, or a fast lane looks
		// like a missing measurement.
		w = minVisibleBarPx
		r = 0
	}

	var d strings.Builder

	fmt.Fprintf(&d, "M%v %vH%v", num(x), num(y), num(x+w-r))

	if r > 0 {
		fmt.Fprintf(&d, "a%v %v 0 0 1 %v %v", num(r), num(r), num(r), num(r))
		fmt.Fprintf(&d, "V%v", num(y+h-r))
		fmt.Fprintf(&d, "a%v %v 0 0 1 %v %v", num(r), num(r), num(-r), num(r))
	} else {
		fmt.Fprintf(&d, "V%v", num(y+h))
	}

	fmt.Fprintf(&d, "H%vZ", num(x))

	fmt.Fprintf(&c.body, "<path d=%q fill=%q%v/>\n", d.String(), fill, extra)
}

func (c *svgCanvas) line(x1, y1, x2, y2 float64, stroke string, width float64, extra string) {
	fmt.Fprintf(&c.body, "<line x1=%q y1=%q x2=%q y2=%q stroke=%q stroke-width=%q%v/>\n",
		num(x1), num(y1), num(x2), num(y2), stroke, num(width), extra)
}

// text writes a label. anchor is "start", "middle" or "end".
func (c *svgCanvas) text(x, y float64, s, fill, anchor string, size float64, extra string) {
	if s == "" {
		return
	}

	var esc bytes.Buffer

	xml.EscapeText(&esc, []byte(s)) //nolint:errcheck // writing to a bytes.Buffer cannot fail.

	fmt.Fprintf(&c.body, "<text x=%q y=%q fill=%q text-anchor=%q font-size=%q%v>%v</text>\n",
		num(x), num(y), fill, anchor, num(size), extra, esc.String())
}

// document wraps the accumulated body in a complete, self-contained SVG.
func (c *svgCanvas) document(title, desc string) string {
	var out bytes.Buffer

	var escTitle, escDesc bytes.Buffer

	xml.EscapeText(&escTitle, []byte(title)) //nolint:errcheck // writing to a bytes.Buffer cannot fail.
	xml.EscapeText(&escDesc, []byte(desc))   //nolint:errcheck // writing to a bytes.Buffer cannot fail.

	fmt.Fprintf(&out, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	fmt.Fprintf(&out, "<svg xmlns=\"http://www.w3.org/2000/svg\" width=%q height=%q viewBox=\"0 0 %v %v\" role=\"img\" aria-labelledby=\"t d\">\n",
		num(c.w), num(c.h), num(c.w), num(c.h))
	fmt.Fprintf(&out, "<title id=\"t\">%v</title>\n<desc id=\"d\">%v</desc>\n", escTitle.String(), escDesc.String())
	out.WriteString(styleBlock)
	out.WriteString(defsBlock)

	// The baked surface. Without it the chart is transparent, and dark ink on a
	// dark GitHub theme is unreadable.
	fmt.Fprintf(&out, "<rect x=\"0\" y=\"0\" width=%q height=%q fill=%q/>\n", num(c.w), num(c.h), colorSurface)

	out.Write(c.body.Bytes())
	out.WriteString("</svg>\n")

	return out.String()
}

// styleBlock is the whole stylesheet: one sans stack and the two states a mark
// can be in.
const styleBlock = `<style>
text{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
.dim{opacity:.45}
</style>
`

// defsBlock holds the hatch used to mark a cell whose measurement is not
// trustworthy. Texture is opt-in here for exactly the reason it is allowed:
// the cell must not read as a value, and color alone would not say so.
const defsBlock = `<defs>
<pattern id="broken" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
<rect width="6" height="6" fill="` + colorSurface + `"/>
<line x1="0" y1="0" x2="0" y2="6" stroke="` + colorCritical + `" stroke-width="3"/>
</pattern>
</defs>
`

// num renders a coordinate without trailing zeros, so the documents diff
// cleanly.
func num(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%.0f", v)
	}

	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

// ---------------------------------------------------------------------------
// axis ticks
// ---------------------------------------------------------------------------

// niceTickMultipliers are the round steps a tick interval is snapped up to,
// within one decade.
var niceTickMultipliers = []float64{1, 2, 2.5, 5, 10}

// niceTicks returns round tick values covering [0, maxValue], aiming for about
// target intervals. Ticks are always clean numbers, because they carry the
// values that are not directly labeled.
func niceTicks(maxValue float64, target int) []float64 {
	if maxValue <= 0 || target < 1 {
		return []float64{0}
	}

	raw := maxValue / float64(target)
	mag := math.Pow(decadeBase, math.Floor(math.Log10(raw)))

	step := mag

	for _, m := range niceTickMultipliers {
		if mag*m >= raw {
			step = mag * m

			break
		}
	}

	// The last tick is at or beyond maxValue, never below it: the top tick is the
	// axis maximum, and a mark drawn past it would leave the plot.
	out := []float64{}

	for v := 0.0; ; v += step {
		out = append(out, v)

		if v >= maxValue {
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// value formatting
// ---------------------------------------------------------------------------

// formatMS renders a millisecond figure the way the reports quote it.
func formatMS(v float64) string {
	switch {
	case v <= 0:
		return "0"
	case v < 1:
		return fmt.Sprintf("%.2f ms", v)
	case v < msPerSecond:
		return fmt.Sprintf("%.0f ms", v)
	case v < secondsCutoffMS:
		return fmt.Sprintf("%.2f s", v/msPerSecond)
	default:
		return fmt.Sprintf("%.1f s", v/msPerSecond)
	}
}

// groupDigits is how many digits a thousands separator groups.
const groupDigits = 3

// formatCount renders an integer with thousands separators.
func formatCount(v float64) string {
	s := fmt.Sprintf("%.0f", math.Abs(v))

	var out []byte

	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%groupDigits == 0 {
			out = append(out, ',')
		}

		out = append(out, c)
	}

	if v < 0 {
		return "-" + string(out)
	}

	return string(out)
}
