//go:build !no_extra_providers

package main

import (
	"math"
	"strings"
)

// The chart canvas. 900px is about the width a GitHub comment renders an image
// at, so the charts are drawn at their reading size rather than scaled down.
const (
	chartWidth = 900.0
	chartPad   = 16.0

	minPlotWidth  = 160.0
	maxLabelWidth = 250.0
	minLabelWidth = 90.0

	titleBaseline    = 28.0
	subtitleLeading  = 15.0
	legendHeight     = 24.0
	panelHeadingRise = 16.0
	panelGap         = 26.0
	axisBandHeight   = 18.0
	rowPadding       = 18.0
	captionLeading   = 14.0

	swatchSize = 11.0
	tickTarget = 5

	// Padding and gaps, all in px. They are named because a bare number in a
	// layout expression is unreadable a month later and the linter is right to
	// object.
	labelGutter     = 14.0 // air between the longest row label and the axis
	labelToAxis     = 10.0 // where a row label's right edge sits before the axis
	valueGutter     = 12.0 // air between a mark's end and its value label
	valueGap        = 7.0  // gap between the whisker cap and the value label
	noteGutter      = 14.0 // air before the right-hand significance annotation
	noteGap         = 12.0 // gap between the last value label and the note
	headerGap       = 6.0  // between the subtitle block and what follows it
	legendSwatchGap = 6.0  // between a legend swatch and its text
	legendItemGap   = 22.0 // between two legend items
	legendLift      = 8.0  // the legend baseline sits this far above the panels
	swatchRise      = 2.0  // how far a swatch sits above the text baseline
	headingDrop     = 5.0  // a panel heading's baseline inside its band
	tickLabelDrop   = 13.0 // a tick label's baseline below the plot
	hairline        = 1.0  // gridline and axis stroke width
	markStroke      = 2.0  // line/whisker stroke width, per the mark spec
	whiskerRing     = 5.0  // the surface-colored underlay under a whisker
	baselineShift   = 0.35 // fraction of the font size that centers text on a mark
	halfDivisor     = 2.0
)

// barValue is one series' bar in one row.
type barValue struct {
	present bool

	median float64
	p10    float64
	p90    float64

	// text is the directly-labeled value at the bar's tip.
	text string

	// broken marks a cell whose measurement is not trustworthy. It is drawn as a
	// hatch rather than as a length, because a wrong number that looks like a
	// good result is worse than a visible hole.
	broken bool
}

// barRow is one category: one bar per series, plus an annotation.
type barRow struct {
	label  string
	values []barValue

	// note rides the right edge: the significance verdict when the row is not
	// REAL, or the accounting tripwire.
	note string

	// dim greys the row's marks, so a difference that is not safe to act on does
	// not look like one.
	dim bool
}

// maxExtent is the largest coordinate the row needs, whiskers included.
func (r barRow) maxExtent() float64 {
	out := 0.0

	for _, v := range r.values {
		if v.present {
			out = math.Max(out, math.Max(v.median, v.p90))
		}
	}

	return out
}

// maxMedian is the row's largest median, which is what decides its magnitude
// band.
func (r barRow) maxMedian() float64 {
	out := 0.0

	for _, v := range r.values {
		if v.present {
			out = math.Max(out, v.median)
		}
	}

	return out
}

// barPanel is one facet: its own linear axis over its own rows.
type barPanel struct {
	heading string
	rows    []barRow
}

func (p barPanel) maxExtent() float64 {
	out := 0.0
	for _, r := range p.rows {
		out = math.Max(out, r.maxExtent())
	}

	return out
}

// barChart is a whole document.
type barChart struct {
	title    string
	subtitle string
	caption  string
	footnote string

	unit   func(float64) string
	series []string
	panels []barPanel
}

// seriesColor assigns the categorical hues in fixed order. The charts never
// carry more than two lanes; a third would need the palette's third slot rather
// than a generated hue, which is why this refuses instead of inventing one.
func seriesColor(i int) string {
	switch i {
	case 0:
		return colorSeries1
	case 1:
		return colorSeries2
	default:
		return colorInk3
	}
}

// renderBarChart draws the whole document and returns the SVG source.
func renderBarChart(c *barChart) string {
	l := layOut(c)
	canvas := newSVGCanvas(chartWidth, l.height)

	y := drawHeader(canvas, c, l)

	for i := range c.panels {
		y = drawPanel(canvas, c, &c.panels[i], l, y)
	}

	drawFooter(canvas, c, l, y)

	return canvas.document(c.title, c.subtitle)
}

// chartLayout is every measurement the drawing depends on, computed once.
type chartLayout struct {
	plotX  float64
	plotW  float64
	height float64

	subtitleLines []string
	footnoteLines []string

	rowHeight float64
	seriesN   int
}

// layOut measures the document. Text cannot be measured at generation time, so
// every text column is sized from approxTextWidth with padding on top and every
// value label is placed OUTSIDE its mark, where an underestimate cannot clip it.
func layOut(c *barChart) *chartLayout {
	l := &chartLayout{seriesN: max(len(c.series), 1)}

	labelW, valueW, noteW := 0.0, 0.0, 0.0

	for _, p := range c.panels {
		for _, r := range p.rows {
			labelW = math.Max(labelW, approxTextWidth(r.label, fontLabel))
			noteW = math.Max(noteW, approxTextWidth(r.note, fontValue))

			for _, v := range r.values {
				if v.present {
					valueW = math.Max(valueW, approxTextWidth(v.text, fontValue))
				}
			}
		}
	}

	labelW = math.Min(math.Max(labelW+labelGutter, minLabelWidth), maxLabelWidth)

	// A value label sits past its whisker's cap, not past its bar, so the column
	// reserved for it has to hold the cap and the gap as well.
	valueW += valueGutter + whiskerCap + valueGap

	if noteW > 0 {
		noteW += noteGutter
	}

	l.plotX = chartPad + labelW
	l.plotW = math.Max(chartWidth-chartPad-valueW-noteW-l.plotX, minPlotWidth)

	textWidth := chartWidth - halfDivisor*chartPad
	l.subtitleLines = wrapText(c.subtitle, textWidth, fontSubtitle)
	l.footnoteLines = wrapText(c.footnote, textWidth, fontCaption)

	l.rowHeight = float64(l.seriesN)*barThickness + float64(l.seriesN-1)*barGap + rowPadding

	h := titleBaseline + float64(len(l.subtitleLines))*subtitleLeading + headerGap

	if len(c.series) > 1 {
		h += legendHeight
	}

	for _, p := range c.panels {
		if p.heading != "" {
			h += panelHeadingRise
		}

		h += float64(len(p.rows))*l.rowHeight + axisBandHeight + panelGap
	}

	h += float64(len(l.footnoteLines))*captionLeading + captionLeading + chartPad

	l.height = math.Ceil(h)

	return l
}

// drawHeader writes the title, the wrapped subtitle and the legend, and returns
// the y the first panel starts at.
func drawHeader(canvas *svgCanvas, c *barChart, l *chartLayout) float64 {
	canvas.text(chartPad, titleBaseline, c.title, colorInk, "start", fontTitle, " font-weight=\"600\"")

	y := titleBaseline + subtitleLeading

	for _, line := range l.subtitleLines {
		canvas.text(chartPad, y, line, colorInk2, "start", fontSubtitle, "")

		y += subtitleLeading
	}

	y += headerGap

	// A legend is always present for two or more series, and never for one: with
	// a single series the title already says what is plotted.
	if len(c.series) > 1 {
		x := chartPad

		for i, s := range c.series {
			canvas.rect(x, y-swatchSize+swatchRise, swatchSize, swatchSize, seriesColor(i), " rx=\"2\"")
			canvas.text(x+swatchSize+legendSwatchGap, y, s, colorInk, "start", fontLabel, "")

			x += swatchSize + legendSwatchGap + approxTextWidth(s, fontLabel) + legendItemGap
		}

		y += legendHeight - legendLift
	}

	return y
}

// drawPanel draws one facet and returns the y after it.
func drawPanel(canvas *svgCanvas, c *barChart, p *barPanel, l *chartLayout, y float64) float64 {
	if p.heading != "" {
		canvas.text(chartPad, y+panelHeadingRise-headingDrop, p.heading, colorInk2, "start", fontLabel, " font-weight=\"600\"")

		y += panelHeadingRise
	}

	maxV := p.maxExtent()
	ticks := niceTicks(maxV, tickTarget)
	axisMax := ticks[len(ticks)-1]

	scale := func(v float64) float64 {
		if axisMax <= 0 {
			return 0
		}

		return v / axisMax * l.plotW
	}

	top := y
	bandHeight := float64(len(p.rows)) * l.rowHeight

	// Gridlines first, so every mark sits on top of them.
	for _, t := range ticks {
		x := l.plotX + scale(t)

		canvas.line(x, top, x, top+bandHeight, colorGrid, hairline, "")
		canvas.text(x, top+bandHeight+tickLabelDrop, c.unit(t), colorInk3, "middle", fontCaption, "")
	}

	canvas.line(l.plotX, top, l.plotX, top+bandHeight, colorAxis, hairline, "")

	for i := range p.rows {
		drawRow(canvas, &p.rows[i], l, scale, top+float64(i)*l.rowHeight)
	}

	return top + bandHeight + axisBandHeight + panelGap
}

// drawRow draws one category's bars, whiskers and labels.
func drawRow(canvas *svgCanvas, r *barRow, l *chartLayout, scale func(float64) float64, top float64) {
	groupH := float64(l.seriesN)*barThickness + float64(l.seriesN-1)*barGap

	mid := top + groupH/halfDivisor

	dim := ""
	if r.dim {
		dim = " class=\"dim\""
	}

	canvas.text(l.plotX-labelToAxis, mid+fontLabel*baselineShift, r.label, colorInk, "end", fontLabel, "")

	labelRight := l.plotX

	for i, v := range r.values {
		barY := top + float64(i)*(barThickness+barGap)

		if !v.present {
			continue
		}

		fill := seriesColor(i)
		if v.broken {
			fill = "url(#broken)"
		}

		w := scale(v.median)
		canvas.bar(l.plotX, barY, w, barThickness, fill, dim)

		if !v.broken {
			drawWhisker(canvas, l.plotX+scale(v.p10), l.plotX+scale(v.p90), barY+barThickness/halfDivisor, dim)
		}

		// The label clears the whisker, not just the bar: p90 reaches past the
		// median, and a value printed on top of its own interval is unreadable.
		tip := l.plotX + math.Max(math.Max(w, minVisibleBarPx), scale(v.p90)+whiskerCap)
		canvas.text(tip+valueGap, barY+barThickness/halfDivisor+fontValue*baselineShift, v.text, colorInk2, "start", fontValue, "")

		labelRight = math.Max(labelRight, tip+valueGap+approxTextWidth(v.text, fontValue))
	}

	if r.note != "" {
		color := colorInk3
		if strings.HasPrefix(r.note, "BROKEN") {
			color = colorCritical
		}

		canvas.text(labelRight+noteGap, mid+fontValue*baselineShift, r.note, color, "start", fontValue, "")
	}
}

// drawWhisker draws the [p10..p90] interval over a bar. It carries a
// surface-colored underlay so it stays legible where it crosses the fill.
func drawWhisker(canvas *svgCanvas, x1, x2, y float64, dim string) {
	if x2 <= x1 {
		return
	}

	canvas.line(x1, y, x2, y, colorSurface, whiskerRing, dim)
	canvas.line(x1, y, x2, y, colorInk2, markStroke, dim)
	canvas.line(x1, y-whiskerCap, x1, y+whiskerCap, colorInk2, markStroke, dim)
	canvas.line(x2, y-whiskerCap, x2, y+whiskerCap, colorInk2, markStroke, dim)
}

// drawFooter writes the wrapped footnote and the provenance caption.
func drawFooter(canvas *svgCanvas, c *barChart, l *chartLayout, y float64) {
	y -= panelGap - captionLeading

	for _, line := range l.footnoteLines {
		canvas.text(chartPad, y, line, colorInk3, "start", fontCaption, "")

		y += captionLeading
	}

	canvas.text(chartPad, y+swatchRise, c.caption, colorInk3, "start", fontCaption, "")
}

// wrapText breaks a string into lines no wider than width. It is deliberately
// crude - approxTextWidth is an estimate - so the lines are laid out with room
// to spare rather than to the pixel.
func wrapText(s string, width, size float64) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	out := []string{}
	line := ""

	for w := range strings.FieldsSeq(s) {
		candidate := w
		if line != "" {
			candidate = line + " " + w
		}

		if line != "" && approxTextWidth(candidate, size) > width {
			out = append(out, line)
			line = w

			continue
		}

		line = candidate
	}

	if line != "" {
		out = append(out, line)
	}

	return out
}
