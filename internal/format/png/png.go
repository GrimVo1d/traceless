// Package png inspects and strips metadata from PNG files.
//
// Stripping rebuilds the file rather than filtering it. Only the chunks
// needed to render the image are kept, and only where a decoder would use
// them: correct size, correct position, no duplicates. The pixel data is
// decompressed, checked against the header, and re-compressed by our own
// encoder, so nothing hidden in or after the compressed stream survives.
// Everything else is streamed; memory use depends on the image width, not the
// file size.
//
// Palette images keep only the palette entries their pixels use: PLTE is cut
// after the last one and zeroed in between, tRNS and bKGD follow. PLTE comes
// before the pixels, so Strip holds back the rebuilt image data, up to
// maxHeld, and writes these chunks first. Pixel indexes past the end of the
// palette are refused.
//
// The EXIF orientation of the first eXIf chunk before the image data is kept:
// Strip writes it as a minimal eXIf chunk that holds nothing else.
//
// Verify is a fixed-point check: it rebuilds the file as Strip would and
// passes only if the input is byte-identical to the result, so neither the
// chunk layout nor the compressed stream can carry anything.
//
// Animated PNG (APNG) is not supported yet and fails closed.
package png

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"traceless/internal/format"
	"traceless/internal/format/exif"
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
func (Handler) Inspect(b *format.Budget, r io.Reader) ([]format.Field, error) {
	p := &processor{mode: modeInspect, budget: b, report: format.Report{Unit: "chunks"}}
	if err := p.run(r); err != nil {
		return nil, err
	}
	return p.report.Result(), nil
}

// Strip implements format.Handler.
func (Handler) Strip(b *format.Budget, r io.Reader, w io.Writer) error {
	p := &processor{mode: modeStrip, budget: b, w: w}
	return p.run(r)
}

// errNotStripped is what Verify returns for a file that holds no metadata
// Inspect can name but isn't what Strip writes.
var errNotStripped = fmt.Errorf("png: %w: chunk layout or image compression differ from a stripped file", format.ErrMetadataRemains)

// Verify implements format.Handler. It rebuilds the file as Strip would and
// fails unless the result is byte-identical to the input.
func (Handler) Verify(b *format.Budget, r io.Reader) error {
	return format.VerifyFixedPoint(b, r, errNotStripped, func(in io.Reader, out io.Writer) error {
		bw := bufio.NewWriter(out)
		p := &processor{mode: modeVerify, budget: b, w: bw}
		if err := p.run(in); err != nil {
			return err
		}
		return bw.Flush()
	})
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
	mode        mode
	budget      *format.Budget
	text        *textDecoder // for Inspect, once needed
	w           io.Writer    // output, nil in modeInspect
	cr          *chunkReader
	layout      layout
	report      format.Report
	pending     []pendingChunk // color space chunks not yet written
	sawEXIF     bool           // an eXIf chunk was seen
	orientation int            // EXIF orientation to keep, 0 if none

	// Palette images: PLTE, tRNS and bKGD are made canonical once the image
	// data has been read (see finishPalette).
	plte, trns, bkgd []byte      // their chunk data
	bkgdFirst        bool        // bKGD came before tRNS
	held             *heldWriter // output of the image data, nil in modeInspect
}

// maxHeld caps the output Strip holds back for a palette image: its image
// data, as re-compressed.
const maxHeld = 64 << 20

// heldWriter takes the output of a palette image's image data. Strip holds it
// back until the pixels show which palette entries are used, since PLTE must
// come first; Verify writes it through. Both apply the same cap.
type heldWriter struct {
	w   io.Writer      // where it goes
	buf *format.Buffer // the output held back, nil to write through
	n   int            // bytes written
}

func (hw *heldWriter) Write(b []byte) (int, error) {
	if hw.n += len(b); hw.n > maxHeld {
		return 0, unsupported("palette image data over %d MB", maxHeld>>20)
	}
	if hw.buf == nil {
		return hw.w.Write(b)
	}
	return hw.buf.Write(b)
}

// release returns the memory held for the buffer.
func (hw *heldWriter) release() {
	if hw.buf != nil {
		hw.buf.Release()
	}
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

	case h.Type == "eXIf" && !p.sawEXIF:
		return p.exif(h)

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
	p.plte = data
	if p.mode == modeStrip {
		return nil // written by finishPalette
	}
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
	palette := l.hdr.colorType == colorPalette
	if palette && p.w != nil {
		hw := &heldWriter{w: p.w}
		if p.mode == modeStrip {
			hw.buf = format.NewBuffer(p.budget)
		}
		defer hw.release()
		p.held, p.w = hw, hw
	}
	if p.orientation != 0 {
		if err := p.write(func() error { return writeChunk(p.w, "eXIf", orientationChunk(p.orientation)) }); err != nil {
			return err
		}
	}
	l.sawImage = true

	ir := newIDATReader(p.cr)
	stats, err := processImage(p.budget, l.hdr, l.paletteLen, ir, p.w)
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
	if stats.paddingRows > 0 {
		if err := p.found("Hidden data", fmt.Sprintf("unused bits set in %d image rows", stats.paddingRows)); err != nil {
			return err
		}
	}
	if palette {
		return p.finishPalette(&stats.used)
	}
	return nil
}

// finishPalette makes PLTE, tRNS and bKGD canonical once the pixels show
// which palette entries they use: PLTE ends at the last used entry and the
// unused ones before it are zeroed, tRNS loses unused entries and trailing
// opaque ones (opaque is the default), and bKGD is dropped unless it names a
// used entry. None of that changes what a viewer shows. Strip then writes
// these chunks and the output it held back; Inspect and Verify report what
// differs from them.
func (p *processor) finishPalette(used *[256]bool) error {
	last := 0
	for i := range p.layout.paletteLen {
		if used[i] {
			last = i
		}
	}
	plte := slices.Clone(p.plte[:3*(last+1)])
	trns := slices.Clone(p.trns[:min(len(p.trns), last+1)])
	for i := range last + 1 {
		if !used[i] {
			clear(plte[3*i : 3*i+3])
			if i < len(trns) {
				trns[i] = 255
			}
		}
	}
	for len(trns) > 0 && trns[len(trns)-1] == 255 {
		trns = trns[:len(trns)-1]
	}
	bkgd := p.bkgd
	if len(bkgd) == 1 && !used[bkgd[0]] {
		if err := p.found("bKGD chunk", "palette entry no pixel uses"); err != nil {
			return err
		}
		bkgd = nil
	}
	if !bytes.Equal(plte, p.plte) {
		// Entries cut or zeroed.
		n := p.layout.paletteLen - (last + 1)
		for i := range last + 1 {
			if !used[i] && slices.ContainsFunc(p.plte[3*i:3*i+3], func(b byte) bool { return b != 0 }) {
				n++
			}
		}
		if err := p.found("Hidden data", fmt.Sprintf("%d palette entries no pixel uses", n)); err != nil {
			return err
		}
	}
	if !bytes.Equal(trns, p.trns) {
		if err := p.found("tRNS chunk", "alpha values that change nothing"); err != nil {
			return err
		}
	}
	if p.mode == modeVerify && p.bkgdFirst && len(trns) > 0 && bkgd != nil {
		return errNotStripped // Strip writes tRNS first
	}

	hw := p.held
	if hw == nil {
		return nil // Inspect writes nothing
	}
	p.w, p.held = hw.w, nil
	if hw.buf == nil {
		return nil // Verify wrote the input's chunks
	}
	if err := writeChunk(p.w, "PLTE", plte); err != nil {
		return err
	}
	if len(trns) > 0 {
		if err := writeChunk(p.w, "tRNS", trns); err != nil {
			return err
		}
	}
	if bkgd != nil {
		if err := writeChunk(p.w, "bKGD", bkgd); err != nil {
			return err
		}
	}
	_, err := p.w.Write(hw.buf.Bytes())
	return err
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
	if l.hdr.colorType == colorPalette && (h.Type == "tRNS" || h.Type == "bKGD") {
		if h.Type == "tRNS" {
			p.trns = data
		} else {
			p.bkgd, p.bkgdFirst = data, p.trns == nil
		}
		if p.mode == modeStrip {
			return nil // written by finishPalette
		}
	}
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

// exif handles the first eXIf chunk; the spec allows only one, and later
// ones are dropped like any other chunk. Viewers rotate the image by the
// Orientation tag of an eXIf chunk before the image data. That orientation is
// kept, and the chunk itself only if it holds nothing else; Strip writes it
// just before the image data.
func (p *processor) exif(h chunkHeader) error {
	p.sawEXIF = true
	var data []byte // nil if too large to decode
	if h.Length <= maxChunkRead {
		// Read what is there rather than allocate the claimed length.
		var err error
		if data, err = io.ReadAll(p.cr.Data()); err != nil {
			return err
		}
		if err := p.cr.Finish(); err != nil {
			return err
		}
	}
	if !p.layout.sawImage {
		p.orientation = exif.Orientation(data)
		if p.orientation < 2 {
			p.orientation = 0 // 1 is the default: no chunk needed
		}
		if p.orientation != 0 && bytes.Equal(data, orientationChunk(p.orientation)) {
			return nil
		}
	}
	switch p.mode {
	case modeVerify:
		return fmt.Errorf("png: %w: eXIf chunk", format.ErrMetadataRemains)
	case modeInspect:
		// Only this chunk is decoded, so repeating a costly chunk can't
		// multiply the work.
		for _, f := range describeEXIF(h, data) {
			p.report.Add(f, false, 0)
		}
	}
	return nil
}

// orientationChunk returns the eXIf data stripped files carry for
// orientation o, from 2 to 8: a minimal EXIF block holding only that tag.
func orientationChunk(o int) []byte {
	return exif.OrientationOnly(uint16(o)) //nolint:gosec // o is 2 to 8
}

// drop handles an ancillary chunk that is never kept.
func (p *processor) drop(h chunkHeader) error {
	switch p.mode {
	case modeVerify:
		return fmt.Errorf("png: %w: %s chunk", format.ErrMetadataRemains, h.Type)
	case modeInspect:
		if p.report.Full() {
			// It would only be counted, so don't decode it.
			p.report.Add(format.Field{Name: h.Type + " chunk"}, true, int64(h.Length))
			return nil
		}
		if p.text == nil {
			p.text = &textDecoder{budget: p.budget}
		}
		f, grouped, err := p.text.describe(h, p.cr.Data())
		if err != nil {
			return err
		}
		p.report.Add(f, grouped, int64(h.Length))
	}
	return nil
}

// found handles metadata that isn't a whole chunk to describe.
func (p *processor) found(name, value string) error {
	switch p.mode {
	case modeVerify:
		return fmt.Errorf("png: %w: %s: %s", format.ErrMetadataRemains, name, value)
	case modeInspect:
		p.report.Add(format.Field{Name: name, Value: value}, false, 0)
	}
	return nil
}

// keep writes a chunk to the output, if there is one.
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
	if p.w == nil {
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
