// Package png inspects and strips metadata from PNG files.
//
// Stripping rebuilds the file rather than filtering it. Only the chunks
// needed to render the image are kept, and only where a decoder would use
// them: correct size, correct position, no duplicates. The pixel data is
// decompressed, checked against the header, and re-compressed by our own
// encoder, so nothing hidden in or after the compressed stream survives.
// Everything is streamed; memory use depends on the image width, not the file
// size.
//
// Animated PNG (APNG) is not supported yet and fails closed.
package png

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"traceless/internal/format"
)

// Handler handles PNG files.
type Handler struct{}

// Name implements format.Handler.
func (Handler) Name() string { return "PNG" }

// Match implements format.Handler.
func (Handler) Match(header []byte) bool {
	return strings.HasPrefix(string(header), signature)
}

// Inspect implements format.Handler.
func (Handler) Inspect(r io.Reader) ([]format.Field, error) {
	p := &processor{mode: modeInspect}
	if err := p.run(r); err != nil {
		return nil, err
	}
	return p.report.result(), nil
}

// Strip implements format.Handler.
func (Handler) Strip(r io.Reader, w io.Writer) error {
	p := &processor{mode: modeStrip, w: w}
	return p.run(r)
}

// Verify implements format.Handler.
func (Handler) Verify(r io.Reader) error {
	p := &processor{mode: modeVerify}
	return p.run(r)
}

type mode int

const (
	modeInspect mode = iota
	modeStrip
	modeVerify
)

// processor walks a PNG once. The three modes share every decision about
// what is metadata and differ only in what they do with it: Inspect reports
// it, Strip leaves it out of the output, Verify fails on it.
type processor struct {
	mode    mode
	w       io.Writer // output, modeStrip only
	cr      *chunkReader
	layout  layout
	report  report
	pending []pendingChunk // color space chunks not yet written
	sawEXIF bool           // an eXIf chunk was decoded
}

type pendingChunk struct {
	h    chunkHeader
	data []byte
}

func (p *processor) run(r io.Reader) error {
	cr, err := newChunkReader(r)
	if err != nil {
		return err
	}
	p.cr = cr
	p.layout.seen = make(map[string]bool)
	if err := p.write(func() error { _, err := io.WriteString(p.w, signature); return err }); err != nil {
		return err
	}

	for {
		h, err := cr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := p.chunk(h); err != nil {
			return err
		}
	}

	n, err := cr.Trailing()
	if err != nil {
		return err
	}
	if n > 0 {
		return p.found("Trailing data", fmt.Sprintf("%d bytes after IEND", n))
	}
	return nil
}

func (p *processor) chunk(h chunkHeader) error {
	l := &p.layout
	switch {
	case h.Type == "IHDR":
		if l.seen["IHDR"] {
			return malformed("duplicate IHDR")
		}
		data, err := p.readSmall(h, 13)
		if err != nil {
			return err
		}
		if l.hdr, err = parseHeader(data); err != nil {
			return err
		}
		return p.keep(h, data)

	case h.Type == "PLTE":
		return p.palette(h)

	case h.Type == "IDAT":
		return p.image()

	case h.Type == "IEND":
		if !l.sawImage {
			return malformed("no image data")
		}
		return p.keep(h, nil)

	case h.critical():
		// A decoder can't render the image without understanding a critical
		// chunk, so we can't strip it safely either.
		return unsupported("unknown critical chunk %s", h.Type)

	case h.Type == "acTL" && !l.sawImage:
		return unsupported("animated PNG (APNG)")

	case renderingChunks[h.Type]:
		return p.rendering(h)
	}
	return p.drop(h)
}

// palette handles PLTE, which is required for palette images, forbidden for
// grayscale and only a suggestion for truecolor images.
func (p *processor) palette(h chunkHeader) error {
	l := &p.layout
	if err := p.flushColorSpace(); err != nil {
		return err
	}
	switch {
	case l.sawPalette:
		return malformed("duplicate PLTE")
	case l.sawImage:
		return malformed("PLTE after image data")
	case l.hdr.colorType == colorGray || l.hdr.colorType == colorGrayAlpha:
		return malformed("PLTE in grayscale image")
	case h.Length == 0 || h.Length%3 != 0 || h.Length > 3*256:
		return malformed("invalid PLTE length %d", h.Length)
	}
	if l.hdr.colorType != colorPalette {
		// A suggested palette for truecolor images: unused for rendering.
		l.sawPalette = true
		return p.found("Suggested palette (PLTE)", fmt.Sprintf("%d colors", h.Length/3))
	}
	entries := int(h.Length / 3)
	if entries > 1<<l.hdr.depth {
		return malformed("PLTE has %d entries for bit depth %d", entries, l.hdr.depth)
	}
	data, err := p.readSmall(h, 3*256)
	if err != nil {
		return err
	}
	l.sawPalette, l.paletteLen = true, entries
	return p.keep(h, data)
}

func (p *processor) image() error {
	l := &p.layout
	if l.sawImage {
		return malformed("IDAT chunks are not consecutive")
	}
	if l.hdr.colorType == colorPalette && !l.sawPalette {
		return malformed("palette image without PLTE")
	}
	if err := p.flushColorSpace(); err != nil {
		return err
	}
	l.sawImage = true

	ir := newIDATReader(p.cr)
	var out io.Writer
	if p.mode == modeStrip {
		out = p.w
	}
	stats, err := processImage(l.hdr, l.paletteLen, ir, out)
	if err != nil {
		return err
	}
	p.cr.Unread(ir.next)

	if stats.extraStream > 0 {
		if err := p.found("Hidden data", fmt.Sprintf("%d bytes after the compressed image stream", stats.extraStream)); err != nil {
			return err
		}
	}
	if stats.extraPixels {
		if err := p.found("Hidden data", "decompressed data after the last image row"); err != nil {
			return err
		}
	}
	if stats.badIndexes > 0 {
		if err := p.found("Hidden data", fmt.Sprintf("%d pixels with out-of-range palette indexes", stats.badIndexes)); err != nil {
			return err
		}
	}
	if stats.paddingRows > 0 {
		return p.found("Hidden data", fmt.Sprintf("unused bits set in %d image rows", stats.paddingRows))
	}
	return nil
}

func (p *processor) rendering(h chunkHeader) error {
	l := &p.layout
	reason := l.checkRendering(h.Type, h.Length)
	if reason != "" {
		return p.found(h.Type+" chunk", fmt.Sprintf("%s (%d bytes)", reason, h.Length))
	}
	data, err := p.readSmall(h, maxRenderingLen)
	if err != nil {
		return err
	}
	if reason := l.checkValues(h.Type, data); reason != "" {
		return p.found(h.Type+" chunk", reason)
	}
	l.seen[h.Type] = true
	if colorSpaceChunks[h.Type] {
		p.pending = append(p.pending, pendingChunk{h, data})
		return nil
	}
	return p.keep(h, data)
}

// flushColorSpace writes the buffered color space chunks, dropping those a
// viewer ignores because a higher-priority chunk is present.
func (p *processor) flushColorSpace() error {
	present := make(map[string]bool, len(p.pending))
	for _, c := range p.pending {
		present[c.h.Type] = true
	}
	for _, c := range p.pending {
		if reason := overridden(c.h.Type, present); reason != "" {
			if err := p.found(c.h.Type+" chunk", reason); err != nil {
				return err
			}
			continue
		}
		if err := p.keep(c.h, c.data); err != nil {
			return err
		}
	}
	p.pending = nil
	return nil
}

// drop handles an ancillary chunk that is never kept.
func (p *processor) drop(h chunkHeader) error {
	switch p.mode {
	case modeVerify:
		return fmt.Errorf("png: %w: %s chunk", format.ErrMetadataRemains, h.Type)
	case modeInspect:
		// The spec allows one eXIf chunk. Only the first is decoded, so
		// repeating a costly chunk can't multiply the work.
		if h.Type == "eXIf" && !p.sawEXIF {
			p.sawEXIF = true
			fields, err := describeEXIF(h, p.cr.Data())
			if err != nil {
				return err
			}
			for _, f := range fields {
				p.report.add(f, false, 0)
			}
			return nil
		}
		f, grouped, err := describe(h, p.cr.Data())
		if err != nil {
			return err
		}
		p.report.add(f, grouped, h.Length)
	}
	return nil
}

// found handles metadata that isn't a whole chunk to describe.
func (p *processor) found(name, value string) error {
	switch p.mode {
	case modeVerify:
		return fmt.Errorf("png: %w: %s: %s", format.ErrMetadataRemains, name, value)
	case modeInspect:
		p.report.add(format.Field{Name: name, Value: value}, false, 0)
	}
	return nil
}

// keep writes a chunk to the output in modeStrip.
func (p *processor) keep(h chunkHeader, data []byte) error {
	l := &p.layout
	l.seen[h.Type] = true
	if h.Type == "IEND" {
		// Validate the CRC before finishing.
		if err := p.cr.Finish(); err != nil {
			return err
		}
	}
	return p.write(func() error { return writeChunk(p.w, h.Type, data) })
}

func (p *processor) write(fn func() error) error {
	if p.mode != modeStrip {
		return nil
	}
	return fn()
}

// readSmall reads a chunk whose valid size is at most limit bytes.
func (p *processor) readSmall(h chunkHeader, limit uint32) ([]byte, error) {
	if h.Length > limit {
		return nil, malformed("%s chunk too long (%d bytes)", h.Type, h.Length)
	}
	return p.cr.ReadAll(h)
}

const (
	// maxFields caps how many fields Inspect returns, so a file with millions
	// of chunks can't make it use unbounded memory.
	maxFields = 256
)

// report collects Inspect fields. Repeated generic chunks are grouped into
// one field, and fields past maxFields are only counted.
type report struct {
	fields  []format.Field
	groups  map[string]*group
	omitted int
}

type group struct {
	index int
	count int
	bytes int64
}

func (r *report) add(f format.Field, grouped bool, size uint32) {
	if grouped {
		if g, ok := r.groups[f.Name]; ok {
			g.count++
			g.bytes += int64(size)
			r.fields[g.index].Value = fmt.Sprintf("%d chunks, %d bytes", g.count, g.bytes)
			return
		}
	}
	if len(r.fields) >= maxFields {
		r.omitted++
		return
	}
	if grouped {
		if r.groups == nil {
			r.groups = make(map[string]*group)
		}
		r.groups[f.Name] = &group{index: len(r.fields), count: 1, bytes: int64(size)}
	}
	r.fields = append(r.fields, f)
}

func (r *report) result() []format.Field {
	if r.omitted > 0 {
		return append(r.fields, format.Field{
			Name:  "More metadata",
			Value: fmt.Sprintf("%d further items not shown", r.omitted),
		})
	}
	return r.fields
}
