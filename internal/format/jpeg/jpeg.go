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
// A sequential image coded in one scan is streamed, one MCU at a time.
// Every block costs at least two bits of input, so work is bounded by the
// input size. Progressive images and images split over several scans are
// decoded into a coefficient buffer for the whole image (capped at
// maxCoefBytes) and written as one sequential scan: their coefficients stay
// the same too. Arithmetic-coded, lossless, hierarchical and 12-bit files
// fail closed, as do progressive images that never finish coding some
// coefficients, since viewers smooth those over.
package jpeg

import (
	"bufio"
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
func (Handler) Inspect(b *format.Budget, r io.Reader) ([]format.Field, error) {
	p := &processor{mode: modeInspect, budget: b, report: format.Report{Unit: "segments"}}
	if err := p.run(r); err != nil {
		return nil, err
	}
	return p.report.Result(), nil
}

// Strip implements format.Handler.
func (Handler) Strip(b *format.Budget, r io.Reader, w io.Writer) error {
	p := &processor{mode: modeStrip, budget: b, out: bufio.NewWriter(w)}
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
func (Handler) Verify(b *format.Budget, r io.Reader) error {
	return format.VerifyFixedPoint(b, r, errNotStripped, func(in io.Reader, out io.Writer) error {
		p := &processor{mode: modeVerify, budget: b, out: bufio.NewWriter(out)}
		if err := p.run(in); err != nil {
			return err
		}
		return p.out.Flush()
	})
}

// errNotStripped is what Verify returns for a file that holds no metadata
// Inspect can name but isn't what Strip writes. Progressive files always
// get it, since Strip writes sequential ones.
var errNotStripped = fmt.Errorf("jpeg: %w: headers or image coding differ from a stripped file", format.ErrMetadataRemains)

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
	budget  *format.Budget
	out     *bufio.Writer // rebuilt file, nil in modeInspect
	mr      *markerReader
	hdr     header
	report  format.Report
	stats   stats
	br      *bitReader
	scanned bool   // a scan was read
	scans   int    // scans read
	coefs   *coefs // the image, if it isn't streamed
	sawEXIF bool   // an EXIF segment was decoded for Inspect
	sawXMP  bool   // an XMP segment was shown for Inspect
}

func (p *processor) run(r io.Reader) error {
	defer func() {
		if p.coefs != nil {
			p.coefs.release()
		}
	}()
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
	if p.hdr.incomplete() {
		return unsupported("image data stops before every coefficient is coded in full")
	}

	n, image, err := mr.trailing()
	if err != nil {
		return err
	}
	if p.coefs != nil {
		if err := p.rebuild(); err != nil {
			return err
		}
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
	case m == markerDAC:
		return unsupported("arithmetic coding")
	case m == markerDNL:
		return unsupported("DNL marker")
	case m == markerDQT && p.scanned:
		// libjpeg keeps the tables of the first scan, Go uses the last.
		return unsupported("quantization table defined between scans")
	case m != markerDQT && m != markerDHT && m != markerDRI && m != markerSOF0 && m != markerSOF1 && m != markerSOF2 && m != markerSOS:
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
	case markerSOF0, markerSOF1, markerSOF2:
		return p.hdr.parseSOF(m, data)
	}
	return p.scan(data)
}

// app handles APPn and COM segments. Before the scan, three of them affect
// how the image is shown and are kept in rebuilt form; everything else is
// metadata.
func (p *processor) app(m byte, data []byte) error {
	hd := &p.hdr
	isJFIF := m == markerAPP0 && len(data) >= jfifLen && strings.HasPrefix(string(data), prefixJFIF)
	isAdobe := m == markerAPP0+14 && len(data) >= adobeLen && strings.HasPrefix(string(data), prefixAdobe)
	if p.scanned {
		if isJFIF || isAdobe {
			// libjpeg sets the colors before the first scan; Go reads
			// these segments up to EOI.
			return unsupported("%s after the first scan", label(m, data))
		}
		return p.drop(m, data)
	}
	switch {
	case isJFIF:
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

	case isAdobe:
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

// scan reads a scan header and the image data after it.
func (p *processor) scan(data []byte) error {
	hd := &p.hdr
	if p.scans++; p.scans > maxScans {
		return unsupported("more than %d scans", maxScans)
	}
	sc, err := hd.parseSOS(data)
	if err != nil {
		return err
	}
	if !p.scanned {
		p.scanned = true
		unusedAdobe, err := hd.colors()
		if err != nil {
			return err
		}
		if unusedAdobe {
			if err := p.found("Adobe segment", "color transform unused for this image"); err != nil {
				return err
			}
		}
		if !hd.progressive && len(sc.comps) == len(hd.frame.comps) {
			return p.stream(sc)
		}
		if p.mode == modeVerify {
			// Strip writes one sequential scan, so this file can't be
			// what it writes.
			return errNotStripped
		}
		if p.coefs, err = newCoefs(hd.frame, p.budget); err != nil {
			return err
		}
	}
	return p.buffer(sc)
}

// encState is the state of the re-encoded scan.
type encState struct {
	bw   *bitWriter // nil when only checking
	pred [4]int32   // DC value of the previous block, per component
	last [4]int32   // DC value of the previous block in the MCU, per component
}

// emit passes one block of the rebuilt image to the encoder. A block that
// lies entirely outside the image is never shown; encoders fill it like
// libjpeg does, with no AC and the DC of the previous block, and anything
// else in it is counted as hidden data and replaced.
func (p *processor) emit(es *encState, ci int, outside bool, blk *block) {
	if outside {
		filler := block{es.last[ci]}
		if *blk != filler {
			p.stats.dummy++
			*blk = filler
		}
	}
	es.last[ci] = blk[0]
	if es.bw != nil {
		enc := 2 * min(ci, 1)
		es.bw.encodeBlock(stdEncoders[enc], stdEncoders[enc+1], &es.pred[ci], blk)
	}
}

// stream decodes the one scan of a sequential image MCU by MCU and, unless
// inspecting, writes the rebuilt headers and encodes each block again right
// away.
func (p *processor) stream(sc scan) error {
	hd := &p.hdr
	var es encState
	if p.out != nil {
		if err := hd.write(p.out); err != nil {
			return err
		}
		es.bw = &bitWriter{w: p.out}
	}
	var blk block
	err := p.decode(sc, func(ci, bx, by int, pred *int32, _ *int32) error {
		c := &hd.frame.comps[ci]
		if err := p.br.decodeBlock(hd.huff[0][c.td], hd.huff[1][c.ta], pred, &blk); err != nil {
			return err
		}
		p.emit(&es, ci, bx >= c.bw || by >= c.bh, &blk)
		return nil
	})
	if err != nil {
		return err
	}
	if es.bw != nil {
		es.bw.flush()
	}
	return nil
}

// buffer decodes one scan into the coefficient buffer.
func (p *processor) buffer(sc scan) error {
	hd := &p.hdr
	var blk block
	return p.decode(sc, func(ci, bx, by int, pred, eobrun *int32) error {
		c := &hd.frame.comps[ci]
		coef, end, err := p.coefs.block(ci, bx, by)
		if err != nil {
			return err
		}
		dc, ac := hd.huff[0][c.td], hd.huff[1][c.ta]
		switch {
		case !hd.progressive:
			if err := p.br.decodeBlock(dc, ac, pred, &blk); err != nil {
				return err
			}
			for k, v := range blk {
				coef[k] = int16(v) //nolint:gosec // decodeBlock checks the range
			}
			return nil
		case sc.ss == 0 && sc.ah == 0:
			return p.br.decodeDCFirst(dc, pred, sc.al, coef)
		case sc.ss == 0:
			return p.br.decodeDCRefine(sc.al, coef)
		case sc.ah == 0:
			return p.br.decodeACFirst(ac, sc.ss, sc.se, sc.al, eobrun, coef, end)
		}
		return p.br.decodeACRefine(ac, sc.ss, sc.se, sc.al, eobrun, coef, end)
	})
}

// rebuild encodes the coefficient buffer as one sequential scan, after the
// rebuilt headers. When inspecting, it only checks the blocks outside the
// image.
func (p *processor) rebuild() error {
	f := p.hdr.frame
	var es encState
	if p.out != nil {
		if err := p.hdr.write(p.out); err != nil {
			return err
		}
		es.bw = &bitWriter{w: p.out}
	}
	all := make([]int, len(f.comps))
	for i := range all {
		all[i] = i
	}
	var blk block
	err := f.blocks(all, nil, func(ci, bx, by int) error {
		if err := p.budget.Work(1); err != nil {
			return err
		}
		c := &f.comps[ci]
		outside := bx >= c.bw || by >= c.bh
		blk = block{}
		if coef := p.coefs.comps[ci].peek(bx, by); coef != nil {
			for k, v := range coef {
				blk[k] = int32(v)
			}
		}
		if outside && blk == (block{}) {
			// Not coded at all: single-component scans skip these blocks.
			blk[0] = es.last[ci]
		}
		p.emit(&es, ci, outside, &blk)
		return nil
	})
	if err != nil {
		return err
	}
	if es.bw != nil {
		es.bw.flush()
	}
	return nil
}

// decode walks the entropy-coded data of a scan, calling fn for each block
// with the DC predictor and end-of-band run of its component. It handles
// restart markers and leaves the marker after the data pending.
func (p *processor) decode(sc scan, fn func(ci, bx, by int, pred, eobrun *int32) error) error {
	hd := &p.hdr
	p.br = &bitReader{r: p.mr.r}
	var preds [4]int32
	var eobrun int32
	restart := func(i int) error {
		ri := hd.restart
		if ri == 0 || i == 0 || i%ri != 0 {
			return nil
		}
		m, err := p.br.end(&p.stats)
		if err != nil {
			return err
		}
		if want := byte(markerRST0 + (i/ri-1)%8); m != want {
			return malformed("%s where %s was expected", markerName(m), markerName(want))
		}
		preds, eobrun = [4]int32{}, 0
		return nil
	}
	err := hd.frame.blocks(sc.comps, restart, func(ci, bx, by int) error {
		if err := p.budget.Work(1); err != nil {
			return err
		}
		return fn(ci, bx, by, &preds[ci], &eobrun)
	})
	if err != nil {
		return err
	}
	m, err := p.br.end(&p.stats)
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
	quant, huff, restart := p.hdr.unused()
	if quant > 0 {
		items = append(items, fmt.Sprintf("%d unused quantization tables", quant))
	}
	if huff > 0 {
		items = append(items, fmt.Sprintf("%d unused Huffman tables", huff))
	}
	if restart > 0 {
		items = append(items, fmt.Sprintf("%d unused restart intervals", restart))
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
