// Package exif decodes EXIF metadata for Inspect. EXIF is a TIFF structure:
// a header, then IFDs (image file directories) of tagged entries that can
// point to further IFDs. JPEG APP1 segments, PNG eXIf chunks, HEIC and TIFF
// files all carry it.
package exif

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"traceless/internal/format"
)

const (
	// maxIFDs caps how many IFDs Parse visits. A photo has at most five
	// (main image, thumbnail, Exif, GPS, interoperability).
	maxIFDs = 32

	// maxEntries caps the entries in one IFD. Real IFDs have fewer than 100.
	maxEntries = 1000

	// maxFields caps the fields Parse returns; further tags are only counted.
	// It is half of what a PNG report holds, so EXIF can't crowd out the
	// file's other metadata.
	maxFields = 128

	// maxValues caps how many numbers are shown for one tag.
	maxValues = 16

	// maxValueLen caps a reported text value, in bytes.
	maxValueLen = 1024
)

// Tags that point to other IFDs.
const (
	tagExifIFD    = 0x8769
	tagGPSIFD     = 0x8825
	tagInteropIFD = 0xa005
	tagSubIFDs    = 0x014a
)

// Tags with special formatting.
const (
	tagThumbnailLen = 0x0202
	tagUserComment  = 0x9286
)

// Field types and their sizes in bytes.
const (
	typeByte      = 1
	typeASCII     = 2
	typeShort     = 3
	typeLong      = 4
	typeRational  = 5
	typeSByte     = 6
	typeUndefined = 7
	typeSShort    = 8
	typeSLong     = 9
	typeSRational = 10
	typeFloat     = 11
	typeDouble    = 12
	typeIFD       = 13
)

var typeSize = map[uint16]uint64{
	typeByte: 1, typeASCII: 1, typeShort: 2, typeLong: 4, typeRational: 8,
	typeSByte: 1, typeUndefined: 1, typeSShort: 2, typeSLong: 4,
	typeSRational: 8, typeFloat: 4, typeDouble: 8, typeIFD: 4,
}

// kind says which tag namespace an IFD uses.
type kind int

const (
	kindMain kind = iota // IFD0, IFD1, Exif and SubIFDs share tag numbers
	kindGPS
	kindInterop
)

// ifdRef is an IFD waiting to be read.
type ifdRef struct {
	offset uint32
	kind   kind
	label  string // prefix for field names, e.g. "Thumbnail "
	chain  int    // position in the IFD0 -> IFD1 -> ... chain, or -1
}

type parser struct {
	data    []byte
	order   binary.ByteOrder
	queue   []ifdRef
	seen    map[uint32]bool
	fields  []format.Field
	omitted int
}

// Parse decodes EXIF data that starts with a TIFF header ("II*\0" or
// "MM\0*") and returns one field per tag. Offsets in the data are relative to
// that header.
//
// Malformed data returns an error wrapping format.ErrMalformed, together with
// the fields read before the problem.
func Parse(data []byte) ([]format.Field, error) {
	p := &parser{data: data, seen: make(map[uint32]bool)}
	if len(data) < 8 {
		return nil, malformed("TIFF header too short")
	}
	switch string(data[:4]) {
	case "II*\x00":
		p.order = binary.LittleEndian
	case "MM\x00*":
		p.order = binary.BigEndian
	default:
		return nil, malformed("bad TIFF header")
	}
	p.queue = append(p.queue, ifdRef{offset: p.order.Uint32(data[4:]), kind: kindMain})
	for len(p.queue) > 0 {
		ref := p.queue[0]
		p.queue = p.queue[1:]
		if err := p.ifd(ref); err != nil {
			return p.result(), err
		}
	}
	return p.result(), nil
}

func (p *parser) ifd(ref ifdRef) error {
	if p.seen[ref.offset] {
		return malformed("IFD loop at offset %d", ref.offset)
	}
	p.seen[ref.offset] = true

	off := uint64(ref.offset)
	if off+2 > uint64(len(p.data)) {
		return malformed("IFD offset %d out of range", off)
	}
	n := uint64(p.order.Uint16(p.data[off:]))
	if n > maxEntries {
		return malformed("IFD with %d entries", n)
	}
	end := off + 2 + 12*n
	if end > uint64(len(p.data)) {
		return malformed("IFD at offset %d truncated", off)
	}

	var gps gpsPosition
	for i := range n {
		e := p.data[off+2+12*i:][:12]
		if ref.kind == kindGPS && gps.collect(e, p.order) {
			continue
		}
		if err := p.entry(ref, e); err != nil {
			return err
		}
	}
	if ref.kind == kindGPS {
		if err := p.gps(ref, &gps); err != nil {
			return err
		}
	}

	// Only the IFD0 chain links to a next IFD. Some writers leave out the
	// final zero offset, so a missing one ends the chain.
	if ref.chain >= 0 && end+4 <= uint64(len(p.data)) {
		if next := p.order.Uint32(p.data[end:]); next != 0 {
			return p.enqueue(ifdRef{offset: next, kind: kindMain, label: chainLabel(ref.chain + 1), chain: ref.chain + 1})
		}
	}
	return nil
}

// enqueue adds an IFD to visit. Visited and queued IFDs together are capped,
// so a file can't make the queue grow by repeating pointers.
func (p *parser) enqueue(ref ifdRef) error {
	if len(p.seen)+len(p.queue) >= maxIFDs {
		return malformed("more than %d IFDs", maxIFDs)
	}
	p.queue = append(p.queue, ref)
	return nil
}

// chainLabel names the IFDs after IFD0: IFD1 holds the thumbnail.
func chainLabel(i int) string {
	if i == 1 {
		return "Thumbnail "
	}
	return fmt.Sprintf("IFD%d ", i)
}

// value returns the data of a 12-byte entry: inline if it fits in 4 bytes,
// otherwise at the offset the entry gives.
func (p *parser) value(e []byte) (tag, typ uint16, count uint32, val []byte, err error) {
	tag, typ, count = p.order.Uint16(e), p.order.Uint16(e[2:]), p.order.Uint32(e[4:])
	size, ok := typeSize[typ]
	if !ok {
		return tag, typ, count, nil, nil
	}
	total := uint64(count) * size
	if total <= 4 {
		return tag, typ, count, e[8 : 8+total], nil
	}
	off := uint64(p.order.Uint32(e[8:]))
	if off+total > uint64(len(p.data)) {
		return tag, typ, count, nil, malformed("tag 0x%04x: value out of range", tag)
	}
	return tag, typ, count, p.data[off : off+total], nil
}

func (p *parser) entry(ref ifdRef, e []byte) error {
	tag, typ, count, val, err := p.value(e)
	if err != nil {
		return err
	}
	if _, ok := typeSize[typ]; !ok {
		p.add(ref.label+tagName(ref.kind, tag), fmt.Sprintf("unknown type %d", typ))
		return nil
	}

	if sub, ok := subIFD(ref, tag, typ); ok {
		if count > maxIFDs {
			return malformed("tag 0x%04x: %d IFD offsets", tag, count)
		}
		for i := range uint64(count) {
			sub.offset = p.order.Uint32(val[4*i:])
			if err := p.enqueue(sub); err != nil {
				return err
			}
		}
		return nil
	}
	if len(p.fields) >= maxFields {
		// Only counted, so don't spend time formatting it.
		p.omitted++
		return nil
	}
	if ref.kind == kindMain {
		switch tag {
		case 0x0111, 0x0144, 0x0201:
			// Strip, tile and thumbnail offsets only locate data.
			return nil
		case tagThumbnailLen:
			// IFD1 is the thumbnail's own IFD and already labeled so.
			p.add(strings.TrimSuffix(ref.label, "Thumbnail ")+"Thumbnail", p.numbers(typ, count, val)+" bytes")
			return nil
		}
	}
	p.add(ref.label+tagName(ref.kind, tag), p.format(ref.kind, tag, typ, count, val))
	return nil
}

// subIFD returns the IFD a pointer tag leads to.
func subIFD(ref ifdRef, tag, typ uint16) (ifdRef, bool) {
	if ref.kind != kindMain || (typ != typeLong && typ != typeIFD) {
		return ifdRef{}, false
	}
	switch tag {
	case tagExifIFD:
		return ifdRef{kind: kindMain, label: ref.label, chain: -1}, true
	case tagGPSIFD:
		return ifdRef{kind: kindGPS, label: ref.label, chain: -1}, true
	case tagInteropIFD:
		return ifdRef{kind: kindInterop, label: ref.label, chain: -1}, true
	case tagSubIFDs:
		return ifdRef{kind: kindMain, label: ref.label + "SubIFD ", chain: -1}, true
	}
	return ifdRef{}, false
}

func (p *parser) add(name, value string) {
	if len(p.fields) >= maxFields {
		p.omitted++
		return
	}
	p.fields = append(p.fields, format.Field{Name: "EXIF " + name, Value: value})
}

func (p *parser) result() []format.Field {
	if p.omitted > 0 {
		return append(p.fields, format.Field{
			Name:  "EXIF",
			Value: fmt.Sprintf("%d further tags not shown", p.omitted),
		})
	}
	return p.fields
}

// format renders a tag's value as text. Text is only scanned up to just past
// maxValueLen: many tags can point at the same large value, so scanning all
// of it each time would cost seconds on a 1 MB input.
func (p *parser) format(k kind, tag, typ uint16, count uint32, val []byte) string {
	switch {
	case k == kindMain && tag >= 0x9c9b && tag <= 0x9c9f:
		// Windows XPTitle, XPComment, XPAuthor, XPKeywords, XPSubject:
		// UTF-16LE whatever the byte order.
		return utf16Text(val, binary.LittleEndian)
	case k == kindMain && tag == tagUserComment:
		return p.userComment(val)
	}
	switch typ {
	case typeASCII:
		return text(bytes.TrimRight(head(val), "\x00"))
	case typeUndefined:
		if t := bytes.TrimRight(head(val), "\x00 "); len(t) > 0 && printable(t) {
			return text(t)
		}
		return fmt.Sprintf("%d bytes", len(val))
	}
	return p.numbers(typ, count, val)
}

// userComment decodes a UserComment: an 8-byte character code, then text.
func (p *parser) userComment(val []byte) string {
	if len(val) < 8 {
		return fmt.Sprintf("%d bytes", len(val))
	}
	code, rest := string(val[:8]), val[8:]
	switch code {
	case "ASCII\x00\x00\x00", "\x00\x00\x00\x00\x00\x00\x00\x00":
		if t := bytes.TrimRight(head(rest), "\x00 "); len(t) == 0 {
			return "(empty)"
		} else if printable(t) {
			return text(t)
		}
	case "UNICODE\x00":
		return utf16Text(rest, p.order)
	}
	return fmt.Sprintf("%d bytes", len(val))
}

// numbers renders numeric values separated by spaces, up to maxValues.
func (p *parser) numbers(typ uint16, count uint32, val []byte) string {
	o := p.order
	n := min(uint64(count), maxValues)
	parts := make([]string, 0, n)
	for i := range n {
		var s string
		switch typ {
		case typeByte, typeUndefined:
			s = strconv.Itoa(int(val[i]))
		case typeSByte:
			s = strconv.Itoa(int(int8(val[i]))) //nolint:gosec // signed type: reinterpret the bits
		case typeShort:
			s = strconv.Itoa(int(o.Uint16(val[2*i:])))
		case typeSShort:
			s = strconv.Itoa(int(int16(o.Uint16(val[2*i:])))) //nolint:gosec // signed type: reinterpret the bits
		case typeLong, typeIFD:
			s = strconv.FormatUint(uint64(o.Uint32(val[4*i:])), 10)
		case typeSLong:
			s = strconv.Itoa(int(int32(o.Uint32(val[4*i:])))) //nolint:gosec // signed type: reinterpret the bits
		case typeRational:
			s = ratio(float64(o.Uint32(val[8*i:])), float64(o.Uint32(val[8*i+4:])))
		case typeSRational:
			s = ratio(float64(int32(o.Uint32(val[8*i:]))), float64(int32(o.Uint32(val[8*i+4:])))) //nolint:gosec // signed type: reinterpret the bits
		case typeFloat:
			s = strconv.FormatFloat(float64(math.Float32frombits(o.Uint32(val[4*i:]))), 'g', -1, 32)
		case typeDouble:
			s = strconv.FormatFloat(math.Float64frombits(o.Uint64(val[8*i:])), 'g', -1, 64)
		}
		parts = append(parts, s)
	}
	s := strings.Join(parts, " ")
	if uint64(count) > n {
		s += fmt.Sprintf(" … (%d values)", count)
	}
	return s
}

func ratio(num, den float64) string {
	if den == 0 {
		return strconv.FormatFloat(num, 'g', -1, 64) + "/0"
	}
	return strconv.FormatFloat(num/den, 'g', 6, 64)
}

// gpsPosition collects the four GPS tags that together give a position.
type gpsPosition struct {
	entries [5][]byte // indexed by tag: 1 LatitudeRef ... 4 Longitude
}

// collect keeps entry e if it is one of the position tags.
func (g *gpsPosition) collect(e []byte, o binary.ByteOrder) bool {
	tag := o.Uint16(e)
	if tag < 1 || tag > 4 {
		return false
	}
	g.entries[tag] = e
	return true
}

// gps reports the collected position as decimal degrees, or, if it can't be
// decoded, the raw tags.
func (p *parser) gps(ref ifdRef, g *gpsPosition) error {
	var vals [5][]byte
	complete := true
	for tag := 1; tag <= 4; tag++ {
		e := g.entries[tag]
		if e == nil {
			complete = false
			continue
		}
		_, typ, count, val, err := p.value(e)
		if err != nil {
			return err
		}
		// Odd tags are the N/S or E/W letter, even tags three rationals.
		if tag%2 == 1 && (typ != typeASCII || count < 1) || tag%2 == 0 && (typ != typeRational || count != 3) {
			complete = false
		}
		vals[tag] = val
	}
	if complete {
		lat, ok1 := p.degrees(vals[2], vals[1][0], 'S', 90)
		lon, ok2 := p.degrees(vals[4], vals[3][0], 'W', 180)
		if ok1 && ok2 {
			p.add(ref.label+"GPS position", fmt.Sprintf("%.6f, %.6f", lat, lon))
			return nil
		}
	}
	for tag := 1; tag <= 4; tag++ {
		if g.entries[tag] != nil {
			if err := p.entry(ref, g.entries[tag]); err != nil {
				return err
			}
		}
	}
	return nil
}

// degrees converts degrees, minutes and seconds to signed decimal degrees.
func (p *parser) degrees(val []byte, ref, negative byte, limit float64) (float64, bool) {
	var d float64
	for i, scale := range []float64{1, 60, 3600} {
		num, den := p.order.Uint32(val[8*i:]), p.order.Uint32(val[8*i+4:])
		if den == 0 {
			return 0, false
		}
		d += float64(num) / float64(den) / scale
	}
	if ref == negative {
		d = -d
	}
	return d, math.Abs(d) <= limit
}

func utf16Text(b []byte, o binary.ByteOrder) string {
	b = b[:min(len(b), 2*maxValueLen)]
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = o.Uint16(b[2*i:])
	}
	return text([]byte(strings.TrimRight(string(utf16.Decode(u)), "\x00")))
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// head returns the start of a value: enough to show it, and one byte more so
// text can tell that it was cut.
func head(b []byte) []byte {
	return b[:min(len(b), maxValueLen+1)]
}

// text replaces invalid UTF-8 in b and clips the result to maxValueLen bytes
// at a character boundary. Replacing first matters: each invalid byte grows
// to three.
func text(b []byte) string {
	s := strings.ToValidUTF8(string(b), "\uFFFD")
	if len(s) <= maxValueLen {
		return s
	}
	cut := maxValueLen
	for !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "… (truncated)"
}

func malformed(msg string, args ...any) error {
	return fmt.Errorf("exif: %w: %s", format.ErrMalformed, fmt.Sprintf(msg, args...))
}
