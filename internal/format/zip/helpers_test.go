package zip

import (
	stdzip "archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"testing"
	"time"

	"traceless/internal/format"
	"traceless/internal/format/jpeg"
	"traceless/internal/format/png"
)

// handler cleans PNG, JPEG and ZIP entries, like the one in core.
func handler() Handler { return Handler{Formats: formats} }

func formats() []format.Handler {
	return []format.Handler{png.Handler{}, jpeg.Handler{}, handler()}
}

func budget() *format.Budget {
	return format.NewBudget(context.Background(), format.DefaultLimits)
}

// The fixtures of the image handlers, with metadata.
func metadataPNG(tb testing.TB) []byte  { return readFile(tb, "../png/testdata/metadata.png") }
func metadataJPEG(tb testing.TB) []byte { return readFile(tb, "../jpeg/testdata/metadata.jpg") }

func readFile(tb testing.TB, path string) []byte {
	tb.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// stripWith strips data with handler h.
func stripWith(tb testing.TB, h format.Handler, data []byte) []byte {
	tb.Helper()
	var out bytes.Buffer
	if err := h.Strip(budget(), bytes.NewReader(data), &out); err != nil {
		tb.Fatalf("%s Strip: %v", h.Name(), err)
	}
	return out.Bytes()
}

// The secrets metadataZIP hides in its own metadata.
var secrets = []string{"archive comment secret", "entry comment secret", "resource fork secret", "folder settings secret"}

var modified = time.Date(2024, 5, 6, 7, 8, 10, 0, time.UTC)

// metadataZIP is an archive as zip tools write it, with metadata in every
// place the handler knows: dates and extended timestamps, a Unix owner,
// permissions, comments, macOS and Windows metadata files, and images with
// their own metadata.
func metadataZIP(tb testing.TB) []byte {
	tb.Helper()
	var buf bytes.Buffer
	zw := stdzip.NewWriter(&buf)
	add := func(fh *stdzip.FileHeader, data []byte) {
		w, err := zw.CreateHeader(fh)
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			tb.Fatal(err)
		}
	}
	file := func(name string, mode os.FileMode) *stdzip.FileHeader {
		fh := &stdzip.FileHeader{Name: name, Method: stdzip.Deflate, Modified: modified}
		fh.SetMode(mode)
		return fh
	}

	photo := file("photo.png", 0o600)
	photo.Comment = "entry comment secret"
	// Info-ZIP's Unix extra field: version 1, uid 501, gid 20.
	photo.Extra = []byte{0x75, 0x78, 11, 0, 1, 4, 0xf5, 1, 0, 0, 4, 20, 0, 0, 0}
	add(photo, metadataPNG(tb))
	add(file("photos/", os.ModeDir|0o755), nil)
	add(file("photos/shot.jpg", 0o644), metadataJPEG(tb))
	add(file("run.sh.png", 0o755), metadataPNG(tb))
	add(file("empty", 0o644), nil)
	add(file("__MACOSX/photos/._shot.jpg", 0o644), []byte("resource fork secret"))
	add(file("photos/.DS_Store", 0o644), []byte("folder settings secret"))
	if err := zw.SetComment("archive comment secret"); err != nil {
		tb.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// cleanZIP is a stripped archive of one clean image.
func cleanZIP(tb testing.TB) []byte {
	tb.Helper()
	return stripWith(tb, handler(), rawZIP("", rawEntry{name: "a.png", data: stripWith(tb, png.Handler{}, metadataPNG(tb))}))
}

// rawEntry is an entry for rawZIP, which writes every field as given.
type rawEntry struct {
	name       string
	data       []byte // uncompressed
	stored     bool   // method 0, otherwise deflate
	flags      uint16 // besides the descriptor flag
	method     uint16 // overrides the method if set
	date, time uint16
	cdate      uint16 // date in the central directory, if different
	extra      []byte // local header
	cextra     []byte // central directory
	comment    []byte
	madeBy     uint16 // defaults to versionMadeBy
	external   uint32 // defaults to mode 0644
	descriptor bool   // sizes and CRC in a data descriptor
	hidden     []byte // appended to the compressed data
	comp       []byte // compressed data, instead of compressing data
	cname      string // name in the central directory, if different
	gap        []byte // written after the entry
}

// rawZIP writes an archive with exactly the given fields and no data
// descriptors unless asked.
func rawZIP(comment string, entries ...rawEntry) []byte {
	var out, cd []byte
	for _, e := range entries {
		method := uint16(methodDeflate)
		comp := deflate(e.data)
		if e.stored {
			method, comp = methodStore, e.data
		}
		if e.method != 0 {
			method = e.method
		}
		if e.comp != nil {
			comp = e.comp
		}
		comp = append(comp, e.hidden...)
		madeBy, external := e.madeBy, e.external
		if madeBy == 0 {
			madeBy = versionMadeBy
		}
		if external == 0 {
			external = 0o100644 << 16
		}
		flags := e.flags
		crc, csize, usize := crc32.ChecksumIEEE(e.data), uint32(len(comp)), uint32(len(e.data))
		offset := uint32(len(out))

		le := binary.LittleEndian
		out = le.AppendUint32(out, sigLocal)
		out = le.AppendUint16(out, versionNeeded)
		if e.descriptor {
			out = le.AppendUint16(out, flags|flagDescriptor)
		} else {
			out = le.AppendUint16(out, flags)
		}
		out = le.AppendUint16(out, method)
		out = le.AppendUint16(out, e.time)
		out = le.AppendUint16(out, e.date)
		if e.descriptor {
			out = append(out, make([]byte, 12)...)
		} else {
			out = le.AppendUint32(out, crc)
			out = le.AppendUint32(out, csize)
			out = le.AppendUint32(out, usize)
		}
		out = le.AppendUint16(out, uint16(len(e.name)))
		out = le.AppendUint16(out, uint16(len(e.extra)))
		out = append(out, e.name...)
		out = append(out, e.extra...)
		out = append(out, comp...)
		if e.descriptor {
			out = appendDescriptor(out, crc, csize, usize)
			flags |= flagDescriptor
		}
		out = append(out, e.gap...)
		cname := e.name
		if e.cname != "" {
			cname = e.cname
		}

		cd = le.AppendUint32(cd, sigCentral)
		cd = le.AppendUint16(cd, madeBy)
		cd = le.AppendUint16(cd, versionNeeded)
		cd = le.AppendUint16(cd, flags)
		cd = le.AppendUint16(cd, method)
		cdate := e.date
		if e.cdate != 0 {
			cdate = e.cdate
		}
		cd = le.AppendUint16(cd, e.time)
		cd = le.AppendUint16(cd, cdate)
		cd = le.AppendUint32(cd, crc)
		cd = le.AppendUint32(cd, csize)
		cd = le.AppendUint32(cd, usize)
		cd = le.AppendUint16(cd, uint16(len(cname)))
		cd = le.AppendUint16(cd, uint16(len(e.cextra)))
		cd = le.AppendUint16(cd, uint16(len(e.comment)))
		cd = le.AppendUint32(cd, 0) // disk, internal attributes
		cd = le.AppendUint32(cd, external)
		cd = le.AppendUint32(cd, offset)
		cd = append(cd, cname...)
		cd = append(cd, e.cextra...)
		cd = append(cd, e.comment...)
	}
	cdOffset := uint32(len(out))
	out = append(out, cd...)
	le := binary.LittleEndian
	out = le.AppendUint32(out, sigEnd)
	out = le.AppendUint32(out, 0)
	out = le.AppendUint16(out, uint16(len(entries)))
	out = le.AppendUint16(out, uint16(len(entries)))
	out = le.AppendUint32(out, uint32(len(cd)))
	out = le.AppendUint32(out, cdOffset)
	out = le.AppendUint16(out, uint16(len(comment)))
	return append(out, comment...)
}

func deflate(b []byte) []byte {
	var buf bytes.Buffer
	fw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	fw.Write(b)
	fw.Close()
	return buf.Bytes()
}

// entries opens b with the standard library and returns each entry's name
// and contents.
func entries(tb testing.TB, b []byte) map[string][]byte {
	tb.Helper()
	zr, err := stdzip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		tb.Fatalf("archive/zip can't open the archive: %v", err)
	}
	out := make(map[string][]byte)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			tb.Fatalf("%s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		if err != nil {
			tb.Fatalf("%s: %v", f.Name, err)
		}
		out[f.Name] = data
	}
	return out
}

func flateWriter(w io.Writer) (*flate.Writer, error) {
	return flate.NewWriter(w, flate.DefaultCompression)
}

func pngHandler() format.Handler { return png.Handler{} }
