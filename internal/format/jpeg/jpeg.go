// Package jpeg inspects and strips metadata from JPEG files.
//
// Stripping rebuilds the file rather than filtering it. The headers are
// written from parsed values: quantization tables the image uses, the frame
// header, and the JFIF density, Adobe color transform and EXIF orientation
// when they affect how the image is shown. Every other segment is dropped.
//
// The compressed image data is decoded to its DCT coefficients and encoded
// again with the standard Huffman tables. This is lossless: the coefficients,
// and so the pixels, stay exactly the same. Nothing hidden in the coding
// survives: padding bits, bytes after the data, restart markers, unused
// table entries, or the content of blocks that lie outside the image.
//
// Everything is streamed, one MCU at a time. Every block costs at least two
// bits of input, so work is bounded by the input size. Progressive,
// arithmetic-coded, lossless, hierarchical and 12-bit files fail closed, as
// do images split over several scans.
package jpeg

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"

	"traceless/internal/format"
)

// Handler handles JPEG files.
type Handler struct{}

// Name implements format.Handler.
func (Handler) Name() string { return "JPEG" }

// Match implements format.Handler. Every JPEG starts with SOI followed by
// another marker.
func (Handler) Match(header []byte) bool {
	return strings.HasPrefix(string(header), "\xff\xd8\xff")
}

// Inspect implements format.Handler.
func (Handler) Inspect(r io.Reader) ([]format.Field, error) {
	p := &processor{mode: modeInspect, report: format.Report{Unit: "segments"}}
	if err := p.run(r); err != nil {
		return nil, err
	}
	return p.report.Result(), nil
}

// Strip implements format.Handler.
func (Handler) Strip(r io.Reader, w io.Writer) error {
	p := &processor{mode: modeStrip, out: bufio.NewWriter(w)}
	if err := p.run(r); err != nil {
		return err
	}
	return p.out.Flush()
}

// Verify implements format.Handler. Besides the checks Inspect reports, it
// requires the file to be exactly what Strip writes. Encoders have many free
// choices (Huffman tables and their symbol order, duplicate codes, header
// bytes decoders skip) that can carry data through every check; only the
// canonical form has none left.
func (Handler) Verify(r io.Reader) error {
	in, out := sha256.New(), sha256.New()
	p := &processor{mode: modeVerify, out: bufio.NewWriter(out)}
	if err := p.run(io.TeeReader(r, in)); err != nil {
		return err
	}
	if err := p.out.Flush(); err != nil {
		return err
	}
	if !bytes.Equal(in.Sum(nil), out.Sum(nil)) {
		return fmt.Errorf("jpeg: %w: headers or image coding differ from a stripped file", format.ErrMetadataRemains)
	}
	return nil
}

type mode int

const (
	modeInspect mode = iota
	modeStrip
	modeVerify
)

// processor walks a JPEG once. The three modes share every decision about
// what is metadata and differ only in what they do with it: Inspect reports
// it, Strip leaves it out of the output, Verify fails on it.
type processor struct {
	mode    mode
	out     *bufio.Writer // rebuilt file, nil in modeInspect
	mr      *markerReader
	hdr     header
	report  format.Report
	stats   stats
	scanned bool // the scan was read
	sawEXIF bool // an EXIF segment was decoded for Inspect
	sawXMP  bool // an XMP segment was shown for Inspect
}

func (p *processor) run(r io.Reader) error {
	mr, err := newMarkerReader(r)
	if err != nil {
		return err
	}
	p.mr = mr
	for {
		m, err := mr.next()
		if err != nil {
			return err
		}
		if m == markerEOI {
			break
		}
		if err := p.marker(m); err != nil {
			return err
		}
	}
	if !p.scanned {
		return malformed("no image data")
	}

	n, image, err := mr.trailing()
	if err != nil {
		return err
	}
	if err := p.hidden(); err != nil {
		return err
	}
	if n > 0 {
		value := fmt.Sprintf("%d bytes after EOI", n)
		if image {
			value += " (another JPEG image)"
		}
		if err := p.found("Trailing data", value); err != nil {
			return err
		}
	}
	if p.out != nil {
		_, err := p.out.Write([]byte{0xff, markerEOI})
		return err
	}
	return nil
}

func (p *processor) marker(m byte) error {
	switch {
	case m >= markerAPP0 && m <= markerAPP15 || m == markerCOM:
		data, err := p.mr.segment(m)
		if err != nil {
			return err
		}
		return p.app(m, data)
	case m == markerSOI:
		return malformed("SOI inside the file")
	case m >= markerRST0 && m <= markerRST7:
		return malformed("%s outside the image data", markerName(m))
	case p.scanned:
		// Only metadata and EOI may follow the one supported scan.
		return unsupported("%s after the image data", markerName(m))
	case m == markerSOF2:
		return unsupported("progressive JPEG")
	case m == markerDAC:
		return unsupported("arithmetic coding")
	case m == markerDNL:
		return unsupported("DNL marker")
	case m != markerDQT && m != markerDHT && m != markerDRI && m != markerSOF0 && m != markerSOF1 && m != markerSOS:
		// Other frame types (lossless, hierarchical, arithmetic) and
		// reserved markers.
		return unsupported("%s", markerName(m))
	}

	data, err := p.mr.segment(m)
	if err != nil {
		return err
	}
	switch m {
	case markerDQT:
		return p.hdr.parseDQT(data)
	case markerDHT:
		return p.hdr.parseDHT(data)
	case markerDRI:
		return p.hdr.parseDRI(data)
	case markerSOF0, markerSOF1:
		if err := p.hdr.parseSOF(data); err != nil {
			return err
		}
		p.hdr.baseline = m == markerSOF0
		return nil
	}
	return p.scan(data)
}

// app handles APPn and COM segments. Before the scan, three of them affect
// how the image is shown and are kept in rebuilt form; everything else is
// metadata.
func (p *processor) app(m byte, data []byte) error {
	hd := &p.hdr
	if p.scanned {
		return p.drop(m, data)
	}
	switch {
	case m == markerAPP0 && len(data) >= jfifLen && strings.HasPrefix(string(data), prefixJFIF):
		if hd.jfif != nil {
			return malformed("duplicate JFIF segment")
		}
		j, tw, th, extra, err := parseJFIF(data)
		if err != nil {
			return err
		}
		hd.jfif = j
		if tw*th > 0 {
			if err := p.found("JFIF thumbnail", fmt.Sprintf("%dx%d pixels", tw, th)); err != nil {
				return err
			}
		}
		if extra > 0 {
			return p.found("Hidden data", fmt.Sprintf("%d bytes after the JFIF header", extra))
		}
		return nil

	case m == markerAPP0+14 && len(data) >= adobeLen && strings.HasPrefix(string(data), prefixAdobe):
		if hd.adobe != nil {
			return malformed("duplicate Adobe segment")
		}
		t := data[11]
		if t > 2 {
			return malformed("Adobe transform %d", t)
		}
		hd.adobe = &t
		if extra := len(data) - adobeLen; extra > 0 {
			return p.found("Hidden data", fmt.Sprintf("%d bytes after the Adobe segment", extra))
		}
		return nil

	case m == markerAPP0+1 && !hd.sawEXIF && strings.HasPrefix(string(data), prefixEXIF):
		// Viewers rotate the image by the first EXIF segment. Its
		// orientation is kept; the segment itself only if it holds
		// nothing else.
		hd.sawEXIF = true
		hd.orientation = exifOrientation(data)
		if isOrientationSegment(data, hd.orientation) {
			return nil
		}
	}
	return p.drop(m, data)
}

// scan reads the scan header and image data. Unless inspecting, it writes
// the rebuilt headers first, then the re-encoded data.
func (p *processor) scan(data []byte) error {
	hd := &p.hdr
	if err := hd.parseSOS(data, hd.baseline); err != nil {
		return err
	}
	p.scanned = true
	if hd.restartDefs > 1 {
		if err := p.found("Hidden data", fmt.Sprintf("%d unused restart intervals", hd.restartDefs-1)); err != nil {
			return err
		}
	}
	quant, huff := hd.unused()
	if quant > 0 {
		if err := p.found("Hidden data", fmt.Sprintf("%d unused quantization tables", quant)); err != nil {
			return err
		}
	}
	if huff > 0 {
		if err := p.found("Hidden data", fmt.Sprintf("%d unused Huffman tables", huff)); err != nil {
			return err
		}
	}
	unusedAdobe, err := hd.colors()
	if err != nil {
		return err
	}
	if unusedAdobe {
		if err := p.found("Adobe segment", "color transform unused for this image"); err != nil {
			return err
		}
	}

	var bw *bitWriter
	if p.out != nil {
		if err := hd.write(p.out); err != nil {
			return err
		}
		bw = &bitWriter{w: p.out}
	}
	if err := p.data(bw); err != nil {
		return err
	}
	if bw != nil {
		bw.flush()
	}
	return nil
}

// data decodes the entropy-coded data MCU by MCU and, if bw is not nil,
// encodes it again.
func (p *processor) data(bw *bitWriter) error {
	f, hd := p.hdr.frame, &p.hdr
	br := &bitReader{r: p.mr.r}
	across, down := f.mcus()
	total := across * down
	var decPred, encPred [4]int32
	var blk block

	for i := range total {
		if ri := hd.restart; ri > 0 && i > 0 && i%ri == 0 {
			m, err := br.end(&p.stats)
			if err != nil {
				return err
			}
			if want := byte(markerRST0 + (i/ri-1)%8); m != want {
				return malformed("%s where %s was expected", markerName(m), markerName(want))
			}
			decPred = [4]int32{}
		}
		mx, my := i%across, i/across
		for ci := range f.comps {
			c := &f.comps[ci]
			dc, ac := hd.huff[0][c.td], hd.huff[1][c.ta]
			h, v := c.h, c.v
			if len(f.comps) == 1 {
				h, v = 1, 1
			}
			var last int32
			for y := range v {
				for x := range h {
					if err := br.decodeBlock(dc, ac, &decPred[ci], &blk); err != nil {
						return err
					}
					if mx*h+x >= c.bw || my*v+y >= c.bh {
						// A block entirely outside the image is never
						// shown. Encoders fill it like libjpeg does: no
						// AC, the DC of the previous block.
						filler := block{last}
						if blk != filler {
							p.stats.dummy++
							blk = filler
						}
					}
					last = blk[0]
					if bw != nil {
						enc := 2 * min(ci, 1)
						bw.encodeBlock(stdEncoders[enc], stdEncoders[enc+1], &encPred[ci], &blk)
					}
				}
			}
		}
	}
	m, err := br.end(&p.stats)
	if err != nil {
		return err
	}
	p.mr.pending = m
	return nil
}

// hidden reports what the walk found hidden around the image data.
func (p *processor) hidden() error {
	st := &p.stats
	st.stray += p.mr.stray
	var items []string
	if st.stray > 0 {
		items = append(items, fmt.Sprintf("%d stray bytes before markers", st.stray))
	}
	if st.extra > 0 {
		items = append(items, fmt.Sprintf("%d bytes after the compressed image data", st.extra))
	}
	if st.padding > 0 {
		items = append(items, fmt.Sprintf("padding bits not set in %d places", st.padding))
	}
	if st.dummy > 0 {
		items = append(items, fmt.Sprintf("%d blocks outside the visible image carry data", st.dummy))
	}
	for _, it := range items {
		if err := p.found("Hidden data", it); err != nil {
			return err
		}
	}
	return nil
}

// drop handles a segment that is never kept.
func (p *processor) drop(m byte, data []byte) error {
	switch p.mode {
	case modeVerify:
		return fmt.Errorf("jpeg: %w: %s", format.ErrMetadataRemains, label(m, data))
	case modeInspect:
		p.describe(m, data)
	}
	return nil
}

// found handles metadata that isn't a whole segment to describe.
func (p *processor) found(name, value string) error {
	switch p.mode {
	case modeVerify:
		return fmt.Errorf("jpeg: %w: %s: %s", format.ErrMetadataRemains, name, value)
	case modeInspect:
		p.report.Add(format.Field{Name: name, Value: value}, false, 0)
	}
	return nil
}
