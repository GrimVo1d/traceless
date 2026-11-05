// Package zip inspects and strips metadata from ZIP archives.
//
// The archive is read once, from the start: each local header and its data,
// then the central directory, which must repeat every local header exactly
// (name, flags, method, CRC, sizes, offset) and in the same order, then the
// end record, which must be the last bytes of the file. Extractors read the
// central directory from the end, so any way the two views could disagree
// (data before the first entry or between entries, extra or missing
// records, a comment holding an end record, trailing data) is refused.
//
// Stripping rebuilds the archive. Every entry's data is decompressed and its
// CRC and sizes checked; files are cleaned by the handler of their own
// format, found by magic bytes, and compressed again with our encoder, so
// nothing hidden in or after a compressed stream survives. The rebuilt
// entries have a fixed date (1980-01-01), no extra fields or comments, mode
// 0644 (0755 if it was executable, and for folders), and a data descriptor
// after the data. macOS and Windows metadata files (__MACOSX/, .DS_Store,
// ._*, Thumbs.db, desktop.ini) are dropped. Entries keep their names and
// order.
//
// Entries whose format can't be cleaned make Strip refuse the archive, like
// mat2's default. Empty files and folders are kept. Encryption, ZIP64,
// split archives, symbolic links, compression methods other than deflate,
// stored entries with a data descriptor (their end can only be found from
// the central directory), and names that are unsafe on Unix or Windows or
// that a case-insensitive file system takes for the same file are refused.
//
// Verify is a fixed-point check, like PNG and JPEG: it rebuilds the archive
// and passes only if the input is byte-identical to the result. Nested
// files are checked with their own handler's Verify, so it streams.
package zip

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"traceless/internal/format"
)

// Handler handles ZIP archives. Formats returns the handlers that clean the
// files inside; it may include this one, and the budget's depth limit stops
// archives nested too deep.
type Handler struct {
	Formats func() []format.Handler
}

// Name implements format.Handler.
func (Handler) Name() string { return "ZIP" }

// Match implements format.Handler: an archive starts with its first local
// header, or with the end record if it is empty.
func (Handler) Match(header []byte) bool {
	s := string(header)
	return strings.HasPrefix(s, "PK\x03\x04") || strings.HasPrefix(s, "PK\x05\x06")
}

// Inspect implements format.Handler.
func (h Handler) Inspect(b *format.Budget, r io.Reader) ([]format.Field, error) {
	p := h.processor(modeInspect, b)
	if err := p.run(r); err != nil {
		return nil, err
	}
	return p.report.Result(), nil
}

// Strip implements format.Handler.
func (h Handler) Strip(b *format.Budget, r io.Reader, w io.Writer) error {
	p := h.processor(modeStrip, b)
	bw := bufio.NewWriter(w)
	p.out = &countWriter{w: bw}
	if err := p.run(r); err != nil {
		return err
	}
	return bw.Flush()
}

// errNotStripped is what Verify returns for an archive that holds no
// metadata Inspect can name but isn't what Strip writes.
var errNotStripped = fmt.Errorf("zip: %w: archive layout or compression differ from a stripped archive", format.ErrMetadataRemains)

// Verify implements format.Handler. It rebuilds the archive as Strip would and
// fails unless the result is byte-identical to the input.
func (h Handler) Verify(b *format.Budget, r io.Reader) error {
	return format.VerifyFixedPoint(b, r, errNotStripped, func(in io.Reader, out io.Writer) error {
		p := h.processor(modeVerify, b)
		bw := bufio.NewWriter(out)
		p.out = &countWriter{w: bw}
		if err := p.run(in); err != nil {
			return err
		}
		return bw.Flush()
	})
}

func (h Handler) processor(m mode, b *format.Budget) *processor {
	var formats []format.Handler
	if h.Formats != nil {
		formats = h.Formats()
	}
	return &processor{mode: m, budget: b, formats: formats, report: format.Report{Unit: "entries"}}
}

type mode int

const (
	modeInspect mode = iota
	modeStrip
	modeVerify
)

// Memory held besides the entries: the record scratch buffer, and the flate
// decoder and encoder (measured: ~50 KB and ~1.1 MB).
const (
	readerMemory = 64 << 10
	writerMemory = 3 << 19
)

// entryMemory is held for each entry until the end: its record, its name in
// the duplicate check, and slice growth.
const entryMemory = 256

// processor walks an archive once. The three modes share every decision about
// what is metadata and differ only in what they do with it: Inspect reports
// it, Strip leaves it out of the output, Verify fails on it.
type processor struct {
	mode    mode
	budget  *format.Budget
	formats []format.Handler
	report  format.Report
	rd      *reader
	scratch []byte
	out     *countWriter // rebuilt archive, nil in modeInspect

	entries []entry
	names   map[string]pathInfo // by fold(name), with the folders names imply
	held    int64               // entry memory held
	cdTail  [20]byte            // the last bytes of the central directory

	fr   io.ReadCloser // reused flate decoder
	fw   *flate.Writer // reused flate encoder
	br   *bufio.Reader // reused reader of entry contents
	time timeRange     // entry dates, for Inspect
}

// entry is what an entry's local header and data established, for checking
// its central directory record and writing the rebuilt one.
type entry struct {
	name              string
	flags, method     uint16
	crc, csize, usize uint32
	offset            uint32
	date, time        uint16
	dir, junk         bool
	exec              bool     // executable, from the central directory
	extraIDs          []uint16 // extra fields of the local header, up to maxExtraIDs

	out struct { // the rebuilt entry
		crc, csize, usize, offset uint32
	}
}

// outFlags and outMethod are what the rebuilt entry uses: folders are
// stored, files (empty ones too) are deflated and followed by a data
// descriptor.
func (e *entry) outFlags() uint16 {
	var f uint16
	if !e.dir {
		f |= flagDescriptor
	}
	if !ascii(e.name) {
		f |= flagUTF8
	}
	return f
}

func (e *entry) outMethod() uint16 {
	if e.dir {
		return methodStore
	}
	return methodDeflate
}

// outMode is the rebuilt entry's Unix mode.
func (e *entry) outMode() uint32 {
	switch {
	case e.dir:
		return 0o40755
	case e.exec:
		return 0o100755
	}
	return 0o100644
}

func (p *processor) run(r io.Reader) error {
	if err := p.budget.Hold(scratchLen); err != nil {
		return err
	}
	defer p.budget.Release(scratchLen)
	defer func() { p.budget.Release(p.held) }()
	defer p.closeCodecs()
	p.scratch = make([]byte, scratchLen)
	p.rd = &reader{r: bufio.NewReader(r)}
	p.names = make(map[string]pathInfo)

	sig, err := p.rd.sig()
	for err == nil && sig == sigLocal {
		if err = p.entry(); err == nil {
			sig, err = p.rd.sig()
		}
	}
	if err != nil {
		return err
	}

	cdStart := p.rd.off - 4
	for i := range p.entries {
		if sig != sigCentral {
			return malformed("%d central directory records for %d entries", i, len(p.entries))
		}
		if err := p.central(&p.entries[i]); err != nil {
			return err
		}
		if sig, err = p.rd.sig(); err != nil {
			return err
		}
	}
	switch sig {
	case sigEnd:
	case sigCentral:
		return malformed("more central directory records than entries")
	case sigEnd64, sigLocator64:
		return unsupported("ZIP64 archive")
	case sigSignature, sigArchiveExtra:
		return unsupported("signed or encrypted central directory")
	default:
		return malformed("unexpected record %08x", sig)
	}
	if err := p.end(cdStart, p.rd.off-4-cdStart); err != nil {
		return err
	}
	return p.finish()
}

// entry handles a local header and the data after it.
func (p *processor) entry() error {
	offset := p.rd.off - 4
	h, err := p.rd.localHeader(p.scratch)
	if err != nil {
		return err
	}
	switch {
	case h.flags&flagEncrypted != 0:
		return unsupported("encrypted entry")
	case h.flags&^knownFlags != 0:
		return unsupported("entry flags %#04x", h.flags)
	case h.method != methodStore && h.method != methodDeflate:
		return unsupported("compression method %d", h.method)
	case h.csize > maxSize || h.usize > maxSize || offset > maxSize:
		return unsupported("ZIP64 entry")
	case h.method == methodStore && h.flags&flagDescriptor != 0:
		return unsupported("stored entry with a data descriptor")
	case len(p.entries) >= maxEntries:
		return unsupported("more than %d entries", maxEntries)
	}
	if err := checkName(h.name, h.flags); err != nil {
		return err
	}
	if err := p.addName(h.name); err != nil {
		return err
	}

	e := entry{
		name: h.name, flags: h.flags, method: h.method, date: h.date, time: h.time,
		offset: uint32(offset), //nolint:gosec // checked against maxSize
		dir:    strings.HasSuffix(h.name, "/"),
		junk:   junk(h.name),
	}
	if !e.junk {
		if err := p.dosTime(h.date, h.time); err != nil {
			return err
		}
		if err := p.extras(&e, h.extra, true); err != nil {
			return err
		}
	}
	if err := p.data(&e, h); err != nil {
		return err
	}
	p.entries = append(p.entries, e)
	return nil
}

// data reads an entry's data, cleans or checks it, and writes the rebuilt
// entry.
func (p *processor) data(e *entry, h localHeader) error {
	descriptor := h.flags&flagDescriptor != 0
	src := &limited{rd: p.rd, n: int64(h.csize)}
	if descriptor {
		src.n = maxSize
	}
	var r io.Reader = src
	if h.method == methodDeflate {
		if err := p.budget.Hold(readerMemory); err != nil {
			return err
		}
		defer p.budget.Release(readerMemory)
		if p.fr == nil {
			p.fr = flate.NewReader(src)
		} else if err := p.fr.(flate.Resetter).Reset(src, nil); err != nil {
			return err
		}
		r = p.fr
	}
	cr := &contentReader{r: r, budget: p.budget, inflate: h.method == methodDeflate}
	if err := p.contents(e, cr); err != nil {
		if cr.err != nil {
			return cr.err // the archive, not the file inside
		}
		return err
	}

	compressed := int64(maxSize) - src.n
	if !descriptor {
		compressed = int64(h.csize) - src.n
		if src.n > 0 {
			if err := p.found("Hidden data", fmt.Sprintf("%d bytes after the compressed data of %s", src.n, quote(e.name))); err != nil {
				return err
			}
			if _, err := io.CopyN(io.Discard, p.rd, src.n); err != nil {
				return readErr(err, "truncated entry data")
			}
		}
	}
	e.crc, e.usize = cr.crc, uint32(cr.n) //nolint:gosec // contentReader stops at maxSize
	e.csize = uint32(compressed)          //nolint:gosec // at most maxSize
	if !descriptor {
		e.csize = h.csize
	}
	if cr.n > maxSize || compressed > maxSize {
		return unsupported("ZIP64 entry")
	}

	crc, csize, usize := h.crc, h.csize, h.usize
	if descriptor {
		var err error
		if crc, csize, usize, err = p.rd.readDescriptor(); err != nil {
			return err
		}
	}
	switch {
	case crc != e.crc:
		return malformed("CRC mismatch in %s", quote(e.name))
	case csize != e.csize || usize != e.usize:
		return malformed("sizes of %s don't match its data", quote(e.name))
	}
	return nil
}

// contents handles the decompressed data of an entry.
func (p *processor) contents(e *entry, cr *contentReader) error {
	if e.junk {
		n, err := io.Copy(io.Discard, cr)
		if err != nil {
			return err
		}
		return p.group("System metadata files", e.name, n)
	}
	if p.br == nil {
		p.br = bufio.NewReader(cr)
	} else {
		p.br.Reset(cr)
	}
	_, err := p.br.Peek(1)
	switch {
	case errors.Is(err, io.EOF):
		return p.writeEntry(e, nil)
	case err != nil:
		return err
	case e.dir:
		return malformed("folder %s holds data", quote(e.name))
	}

	h, err := format.Detect(p.br, p.formats)
	if errors.Is(err, format.ErrUnknownFormat) {
		if p.mode != modeInspect {
			return unsupported("%s: no handler can clean this format", quote(e.name))
		}
		if _, err := io.Copy(io.Discard, p.br); err != nil {
			return err
		}
		return p.group("Entries that can't be cleaned", e.name, cr.n)
	}
	if err != nil {
		return err
	}
	leave, err := p.budget.Enter()
	if err != nil {
		return err
	}
	defer leave()

	if p.mode == modeInspect {
		if err := p.inspectEntry(e, h); err != nil {
			return err
		}
	} else if err := p.writeEntry(e, func(w io.Writer) error { return p.clean(e, h, w) }); err != nil {
		return err
	}
	// The handler stops at the end of its format; what is left was hidden
	// after it.
	n, err := io.Copy(io.Discard, p.br)
	if err != nil {
		return err
	}
	if n > 0 {
		return p.found("Hidden data", fmt.Sprintf("%d bytes after the %s file %s", n, h.Name(), quote(e.name)))
	}
	return nil
}

// clean writes an entry's clean contents to w. Strip cleans them with h.
// Verify checks with h that they are clean already and passes them on as
// they are: a nested Strip can hold its output back (a palette PNG holds
// its pixels) further than the fixed-point check of the archive can lag.
// Either way w gets h.Strip's output, since a file passes h.Verify only if
// it is byte-identical to it.
func (p *processor) clean(e *entry, h format.Handler, w io.Writer) error {
	var err error
	if p.mode == modeVerify {
		err = h.Verify(p.budget, io.TeeReader(p.br, w))
	} else {
		err = h.Strip(p.budget, p.br, w)
	}
	if err != nil {
		return fmt.Errorf("zip: %s: %w", quote(e.name), err)
	}
	return nil
}

func (p *processor) inspectEntry(e *entry, h format.Handler) error {
	if p.report.Full() {
		// Its fields would only be counted.
		p.report.Add(format.Field{Name: "Entries not inspected"}, true, 0)
		_, err := io.Copy(io.Discard, p.br)
		return err
	}
	fields, err := h.Inspect(p.budget, p.br)
	if err != nil {
		return fmt.Errorf("zip: %s: %w", quote(e.name), err)
	}
	for _, f := range fields {
		p.report.Add(format.Field{Name: clip(e.name, maxReportName) + ": " + f.Name, Value: f.Value}, false, 0)
	}
	return nil
}

// writeEntry writes the rebuilt entry: its local header, then, for a file,
// what clean writes, compressed and followed by a data descriptor.
func (p *processor) writeEntry(e *entry, clean func(w io.Writer) error) error {
	if p.out == nil {
		return nil
	}
	if p.out.n > maxSize {
		return unsupported("rebuilt archive over 4 GB")
	}
	e.out.offset = uint32(p.out.n) //nolint:gosec // checked against maxSize
	if _, err := p.out.Write(appendLocal(nil, e.name, e.outFlags(), e.outMethod())); err != nil {
		return err
	}
	if e.dir {
		return nil
	}
	if p.fw == nil {
		if err := p.budget.Hold(writerMemory); err != nil {
			return err
		}
		fw, err := flate.NewWriter(p.out, flate.DefaultCompression)
		if err != nil {
			return err
		}
		p.fw = fw
	} else {
		p.fw.Reset(p.out)
	}
	start := p.out.n
	cw := &crcWriter{w: p.fw}
	if clean != nil {
		if err := clean(cw); err != nil {
			return fmt.Errorf("zip: %s: %w", quote(e.name), err)
		}
	}
	if err := p.fw.Close(); err != nil {
		return err
	}
	csize := p.out.n - start
	if cw.n > maxSize || csize > maxSize {
		return unsupported("rebuilt entry %s over 4 GB", quote(e.name))
	}
	e.out.crc, e.out.csize, e.out.usize = cw.crc, uint32(csize), uint32(cw.n) //nolint:gosec // checked against maxSize
	_, err := p.out.Write(appendDescriptor(nil, e.out.crc, e.out.csize, e.out.usize))
	return err
}

// central checks an entry's central directory record against what its local
// header and data established.
func (p *processor) central(e *entry) error {
	c, err := p.rd.centralHeader(p.scratch)
	if err != nil {
		return err
	}
	switch {
	case c.csize > maxSize || c.usize > maxSize || c.offset > maxSize || c.disk == 0xffff:
		return unsupported("ZIP64 entry")
	case c.name != e.name:
		return malformed("central directory names %s where the entry is %s", quote(c.name), quote(e.name))
	case c.flags != e.flags || c.method != e.method || c.crc != e.crc ||
		c.csize != e.csize || c.usize != e.usize || c.offset != e.offset || c.disk != 0:
		return malformed("central directory record of %s differs from its local header", quote(e.name))
	}
	if c.madeBy>>8 == hostUnix {
		mode := c.external >> 16
		switch mode & 0o170000 {
		case 0:
		case 0o40000:
			if !e.dir {
				return malformed("%s is a folder without a trailing slash", quote(e.name))
			}
		case 0o100000:
			if e.dir {
				return malformed("folder %s has a file mode", quote(e.name))
			}
		case 0o120000:
			return unsupported("symbolic link %s", quote(e.name))
		default:
			return unsupported("special file %s", quote(e.name))
		}
		e.exec = !e.dir && mode&0o111 != 0
		if !e.junk && mode != 0 && mode != e.outMode() {
			if err := p.group(fmt.Sprintf("Permissions %04o", mode&0o7777), e.name, 0); err != nil {
				return err
			}
		}
	}
	p.cdTail = c.tail
	if e.junk {
		return nil
	}
	if c.date != e.date || c.time != e.time {
		if err := p.dosTime(c.date, c.time); err != nil {
			return err
		}
	}
	if c.madeBy != versionMadeBy {
		if err := p.group("Archiver: "+archiver(c.madeBy), e.name, 0); err != nil {
			return err
		}
	}
	if c.madeBy>>8 != hostUnix && c.external&0xff&^0x10 != 0 {
		if err := p.group(fmt.Sprintf("DOS attributes %#02x", c.external&0xff), e.name, 0); err != nil {
			return err
		}
	}
	if err := p.extras(e, c.extra, false); err != nil {
		return err
	}
	if len(c.comment) > 0 {
		return p.found("Comment on "+clip(e.name, maxReportName), text(c.comment))
	}
	return nil
}

// end checks the end record against the central directory just read.
func (p *processor) end(cdStart, cdSize int64) error {
	er, err := p.rd.endRecord(p.scratch)
	if err != nil {
		return err
	}
	n := len(p.entries)
	switch {
	case er.entries == 0xffff || er.cdSize == 0xffffffff || er.cdOffset == 0xffffffff:
		return unsupported("ZIP64 archive")
	case er.disk != 0 || er.cdDisk != 0:
		return unsupported("split archive")
	case int(er.entries) != n || int(er.diskEntries) != n:
		return malformed("end record counts %d entries, the archive has %d", er.entries, n)
	case int64(er.cdOffset) != cdStart || int64(er.cdSize) != cdSize:
		return malformed("end record doesn't point at the central directory")
	case holdsEnd(er.fixed[:], er.comment):
		// Extractors look for the last end record in the file.
		return malformed("end record holds another one")
	case n > 0 && binary.LittleEndian.Uint32(p.cdTail[:]) == sigLocator64:
		// Python's zipfile looks for a ZIP64 locator right before the end
		// record, whatever the end record says.
		return malformed("central directory ends with a ZIP64 locator")
	}
	if len(er.comment) > 0 {
		if err := p.found("Archive comment", text(er.comment)); err != nil {
			return err
		}
	}
	if _, err := p.rd.r.Peek(1); err == nil {
		return malformed("data after the end record")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// finish reports the entry dates and writes the rebuilt central directory
// and end record.
func (p *processor) finish() error {
	if p.time.n > 0 {
		if err := p.found("Entry dates", p.time.String()); err != nil {
			return err
		}
	}
	if p.out == nil {
		return nil
	}
	cdStart := p.out.n
	kept := 0
	for i := range p.entries {
		e := &p.entries[i]
		if e.junk {
			continue
		}
		kept++
		if _, err := p.out.Write(appendCentral(p.scratch[:0], e)); err != nil {
			return err
		}
	}
	cdSize := p.out.n - cdStart
	if p.out.n > maxSize {
		return unsupported("rebuilt archive over 4 GB")
	}
	_, err := p.out.Write(appendEnd(p.scratch[:0], kept, uint32(cdSize), uint32(cdStart))) //nolint:gosec // checked against maxSize
	return err
}

func (p *processor) closeCodecs() {
	if p.fw != nil {
		p.budget.Release(writerMemory)
	}
}

// found handles a piece of metadata.
func (p *processor) found(name, value string) error {
	switch p.mode {
	case modeVerify:
		return fmt.Errorf("zip: %w: %s: %s", format.ErrMetadataRemains, name, value)
	case modeInspect:
		p.report.Add(format.Field{Name: name, Value: value}, false, 0)
	}
	return nil
}

// group handles metadata that repeats across entries: Inspect reports one
// field per name with a count, naming the first entry.
func (p *processor) group(name, entry string, size int64) error {
	if p.mode == modeVerify {
		return fmt.Errorf("zip: %w: %s: %s", format.ErrMetadataRemains, name, quote(entry))
	}
	if p.mode == modeInspect {
		p.report.Add(format.Field{Name: name, Value: clip(entry, maxReportName)}, true, size)
	}
	return nil
}

// checkName refuses names an extractor could write outside its target folder
// or read differently from us, on Unix or Windows.
func checkName(name string, flags uint16) error {
	switch {
	case name == "":
		return malformed("entry without a name")
	case strings.ContainsAny(name, "\x00\\"):
		// Without NUL bytes, the last name of the central directory can't
		// forge a ZIP64 locator or end record (their disk numbers are zero)
		// in front of the rebuilt end record.
		return unsupported("entry name %s with a NUL or backslash", quote(name))
	case strings.HasPrefix(name, "/"):
		return unsupported("absolute entry name %s", quote(name))
	case !ascii(name) && flags&flagUTF8 == 0:
		// Without the flag the name is CP437 or a local code page.
		return unsupported("entry name %s in a legacy encoding", quote(name))
	case !utf8.ValidString(name):
		return malformed("entry name %s isn't UTF-8", quote(name))
	}
	for _, c := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		switch {
		case c == "" || c == "." || c == "..":
			return unsupported("entry name %s with an empty, . or .. part", quote(name))
		case strings.ContainsRune(c, ':'):
			// A drive letter or an NTFS stream on Windows.
			return unsupported("entry name %s with a colon", quote(name))
		case strings.HasSuffix(c, ".") || strings.HasSuffix(c, " "):
			// Windows drops them, so the name changes.
			return unsupported("entry name %s with a part ending in a dot or space", quote(name))
		case device(c):
			return unsupported("entry name %s with a Windows device name", quote(name))
		}
	}
	return nil
}

// device reports whether Windows opens a file named c as a device, whatever
// its extension: CON, NUL, COM1 and the others.
func device(c string) bool {
	stem, _, _ := strings.Cut(c, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT") {
		switch stem[3:] {
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "\u00b9", "\u00b2", "\u00b3":
			return true
		}
	}
	return false
}

// pathInfo is what the names of an archive say about one path.
type pathInfo struct {
	name string // the entry with this path, "" for a folder only deeper entries imply
	dir  bool
}

// prefixMemory is held for each folder that only deeper entries imply.
const prefixMemory = 64

// addName records an entry's name and the folders it implies. It refuses a
// name an extractor could take for one it has seen: the same name, the same
// one in another case or Unicode normalization, or a file where a folder is
// (or the other way round). Folders it implies are looked up deepest first, until one is
// known, and each lookup counts as work.
func (p *processor) addName(name string) error {
	dir := strings.HasSuffix(name, "/")
	key := fold(name)
	n := entryMemory + int64(len(name)+len(key))
	if err := p.budget.Hold(n); err != nil {
		return err
	}
	p.held += n

	prev, seen := p.names[key]
	switch {
	case !seen:
	case prev.name == name:
		return malformed("duplicate entry %s", quote(name))
	case prev.dir != dir:
		return unsupported("%s is both a file and a folder", quote(name))
	case prev.name != "":
		return unsupported("entries %s and %s are the same file on macOS and Windows", quote(prev.name), quote(name))
	}
	p.names[key] = pathInfo{name: name, dir: dir}

	for i := strings.LastIndexByte(key, '/'); i > 0; i = strings.LastIndexByte(key[:i], '/') {
		parent := key[:i]
		if err := p.budget.Work(int64(len(parent))); err != nil {
			return err
		}
		if prev, seen := p.names[parent]; seen {
			if !prev.dir {
				return unsupported("%s is both a file and a folder", quote(prev.name))
			}
			break
		}
		if err := p.budget.Hold(prefixMemory); err != nil {
			return err
		}
		p.held += prefixMemory
		p.names[parent] = pathInfo{dir: true}
	}
	return nil
}

// fold maps names that a case-insensitive file system takes for the same
// path to one key, without the trailing slash of a folder. macOS also takes
// names that differ only in Unicode normalization for one (NFC "café" and
// the NFD one it writes), so the key is decomposed before and after the case
// mapping, like Unicode's canonical caseless match. The key can be up to
// three times as long as the name.
func fold(name string) string {
	return norm.NFD.String(strings.ToLower(strings.ToUpper(norm.NFD.String(strings.TrimSuffix(name, "/")))))
}

// holdsEnd reports whether an end record signature starts anywhere in the
// end record after its own: in its fixed fields, its comment, or across the
// comment length into the comment.
func holdsEnd(fixed, comment []byte) bool {
	sig := []byte("PK\x05\x06")
	seam := append(append([]byte(nil), fixed[len(fixed)-3:]...), comment[:min(3, len(comment))]...)
	return bytes.Contains(fixed, sig) || bytes.Contains(seam, sig) || bytes.Contains(comment, sig)
}

// junk reports whether name is a file systems create for their own
// metadata: macOS resource forks and folder settings, Windows thumbnail
// caches and folder settings.
func junk(name string) bool {
	trimmed := strings.TrimSuffix(name, "/")
	for _, c := range strings.Split(trimmed, "/") {
		if c == "__MACOSX" {
			return true
		}
	}
	base := path.Base(trimmed)
	return base == ".DS_Store" || strings.HasPrefix(base, "._") ||
		strings.EqualFold(base, "Thumbs.db") || strings.EqualFold(base, "desktop.ini")
}

func ascii(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// contentReader reads an entry's decompressed data, computing its CRC and
// size and counting it in the budget. It keeps the first error, so a failure
// of the archive can be told apart from one of the file inside.
type contentReader struct {
	r       io.Reader
	budget  *format.Budget
	inflate bool // count the bytes as decompressed
	crc     uint32
	n       int64
	err     error
}

func (cr *contentReader) Read(b []byte) (int, error) {
	if cr.err != nil {
		return 0, cr.err
	}
	n, err := cr.r.Read(b)
	cr.crc = crc32.Update(cr.crc, crc32.IEEETable, b[:n])
	cr.n += int64(n)
	if cr.inflate && n > 0 {
		if berr := cr.budget.Inflate(int64(n)); berr != nil {
			cr.err = berr
			return n, berr
		}
	}
	if cr.n > maxSize {
		cr.err = unsupported("entry over 4 GB")
		return n, cr.err
	}
	var corrupt flate.CorruptInputError
	switch {
	case errors.As(err, &corrupt):
		cr.err = malformed("corrupt compressed data: %v", err)
	case err != nil && !errors.Is(err, io.EOF):
		cr.err = readErr(err, "truncated entry data")
	}
	if cr.err != nil {
		return n, cr.err
	}
	return n, err
}

// crcWriter computes the CRC and size of what is written through it.
type crcWriter struct {
	w   io.Writer
	crc uint32
	n   int64
}

func (cw *crcWriter) Write(b []byte) (int, error) {
	n, err := cw.w.Write(b)
	cw.crc = crc32.Update(cw.crc, crc32.IEEETable, b[:n])
	cw.n += int64(n)
	return n, err
}

// countWriter counts what is written, for the offsets of the rebuilt archive.
type countWriter struct {
	w io.Writer
	n int64
}

func (cw *countWriter) Write(b []byte) (int, error) {
	n, err := cw.w.Write(b)
	cw.n += int64(n)
	return n, err
}
