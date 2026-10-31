package png

import (
	"bufio"
	"compress/flate"
	"compress/zlib"
	"errors"
	"io"
	"math"

	"traceless/internal/format"
)

// idatSize is the data size of the IDAT chunks Strip writes.
const idatSize = 64 << 10

// imageStats records what the pixel stream holds besides the pixels.
type imageStats struct {
	paddingRows int64 // rows with non-zero unused bits in their last byte
	badIndexes  int64 // palette indexes out of range (see fixIndexes)
	extraPixels bool  // decompressed data after the last row
	extraStream int64 // compressed bytes after the end of the zlib stream
}

// maxEmptyReads is how many reads in a row may return no data and no error
// before idatReader gives up, so a broken reader can't make it spin forever.
const maxEmptyReads = 100

// idatReader reads the data of a run of consecutive IDAT chunks as one
// stream. It implements io.ByteReader so the zlib reader consumes exactly its
// own stream and nothing after it.
type idatReader struct {
	cr       *chunkReader
	buf      []byte
	pos, end int
	next     chunkHeader // first chunk after the run, once done
	done     bool
}

func newIDATReader(cr *chunkReader) *idatReader {
	return &idatReader{cr: cr, buf: make([]byte, 32<<10)}
}

// fill loads the next block of IDAT data, moving on to the next chunk when
// the current one is used up. It returns io.EOF after the last IDAT chunk.
func (ir *idatReader) fill() error {
	empty := 0
	for !ir.done {
		n, err := ir.cr.Data().Read(ir.buf)
		if n > 0 {
			ir.pos, ir.end = 0, n
			return nil
		}
		if err == nil {
			if empty++; empty >= maxEmptyReads {
				return io.ErrNoProgress
			}
			continue
		}
		empty = 0
		if err == io.EOF {
			h, err := ir.cr.Next()
			if err != nil {
				return err
			}
			if h.Type != "IDAT" {
				ir.next, ir.done = h, true
			}
		} else if err != nil {
			return err
		}
	}
	return io.EOF
}

func (ir *idatReader) Read(p []byte) (int, error) {
	if ir.pos == ir.end {
		if err := ir.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, ir.buf[ir.pos:ir.end])
	ir.pos += n
	return n, nil
}

func (ir *idatReader) ReadByte() (byte, error) {
	if ir.pos == ir.end {
		if err := ir.fill(); err != nil {
			return 0, err
		}
	}
	ir.pos++
	return ir.buf[ir.pos-1], nil
}

// drain skips the rest of the IDAT run and returns how many bytes it skipped.
func (ir *idatReader) drain() (int64, error) {
	var n int64
	for {
		n += int64(ir.end - ir.pos)
		ir.pos = ir.end
		if err := ir.fill(); err == io.EOF {
			return n, nil
		} else if err != nil {
			return n, err
		}
	}
}

// processImage reads the pixel stream of a run of IDAT chunks and checks it
// against the header. If w is not nil, it writes the pixels as new IDAT
// chunks: re-filtered and re-compressed by our own encoder, with unused bits
// zeroed. That drops anything hidden in the stream and the original
// encoder's fingerprint (compression settings, filter choices).
//
// paletteLen is the number of PLTE entries for palette images.
//
// Memory use is a few rows, bounded by maxRowBytes.
func processImage(h header, paletteLen int, ir *idatReader, w io.Writer) (imageStats, error) {
	var stats imageStats
	zr, err := zlib.NewReader(ir)
	if err != nil {
		return stats, imageErr(err)
	}
	defer zr.Close()

	var cw *chunkWriter
	var zw *zlib.Writer
	var bw *bufio.Writer // batches rows: per-row compressor calls are slow
	if w != nil {
		cw = newChunkWriter(w, "IDAT", idatSize)
		zw = zlib.NewWriter(cw)
		bw = bufio.NewWriterSize(zw, 64<<10)
	}

	bits := h.bitsPerPixel()
	off := h.filterOffset()
	full := rowBytes(h.width, bits)
	raw := make([]byte, 1+full)   // filter byte + row as stored
	prevRaw := make([]byte, full) // previous row, unfiltered, as stored
	var clean, prevClean []byte   // rows with unused bits zeroed
	var filtered [5][]byte
	// Palette indexes and sub-byte samples compress badly after filtering.
	adaptive := h.colorType != colorPalette && h.depth >= 8
	if zw != nil {
		clean, prevClean = make([]byte, full), make([]byte, full)
		filtered[0] = make([]byte, 1+full)
		if adaptive {
			for i := 1; i < len(filtered); i++ {
				filtered[i] = make([]byte, 1+full)
			}
		}
	}
	palette := h.colorType == colorPalette

	for _, p := range h.passes() {
		if p.width == 0 || p.height == 0 {
			continue
		}
		n := int(rowBytes(p.width, bits))
		clear(prevRaw[:n])
		clear(prevClean)
		for range p.height {
			row := raw[:1+n]
			if _, err := io.ReadFull(zr, row); err != nil {
				return stats, imageErr(err)
			}
			if err := unfilter(row[0], row[1:], prevRaw[:n], off); err != nil {
				return stats, err
			}
			if zw != nil {
				copy(clean, row[1:])
				if zeroPadding(clean[:n], p.width, bits) {
					stats.paddingRows++
				}
				if palette {
					stats.badIndexes += fixIndexes(clean[:n], p.width, h.depth, paletteLen, true)
				}
				ft := 0
				if adaptive {
					ft = chooseFilter(&filtered, clean[:n], prevClean[:n], off)
				} else {
					applyFilter(0, filtered[0][:1+n], clean[:n], prevClean[:n], off)
				}
				if _, err := bw.Write(filtered[ft][:1+n]); err != nil {
					return stats, err
				}
				copy(prevClean, clean[:n])
			} else {
				if hasPadding(row[1:], p.width, bits) {
					stats.paddingRows++
				}
				if palette {
					stats.badIndexes += fixIndexes(row[1:], p.width, h.depth, paletteLen, false)
				}
			}
			copy(prevRaw, row[1:])
		}
	}

	// The stream must end exactly here. One more decompressed byte is enough
	// to know there is hidden data; decompressing the rest would only cost
	// time. Data after the zlib stream is counted, not copied.
	var one [1]byte
	if n, err := io.ReadFull(zr, one[:]); n > 0 {
		stats.extraPixels = true
	} else if err != io.EOF {
		return stats, imageErr(err)
	}
	if stats.extraStream, err = ir.drain(); err != nil {
		return stats, err
	}

	if zw != nil {
		if err := bw.Flush(); err != nil {
			return stats, err
		}
		if err := zw.Close(); err != nil {
			return stats, err
		}
		if err := cw.Flush(); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// imageErr maps decompression errors to ErrMalformed and passes others through.
func imageErr(err error) error {
	var corrupt flate.CorruptInputError
	switch {
	case errors.Is(err, format.ErrMalformed):
		return err
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return malformed("truncated image data")
	case errors.Is(err, zlib.ErrChecksum), errors.Is(err, zlib.ErrHeader),
		errors.Is(err, zlib.ErrDictionary), errors.As(err, &corrupt):
		return malformed("corrupt image data: %v", err)
	}
	return err
}

// unusedBits returns a mask of the unused low bits in a row's last byte.
func unusedBits(width uint32, bitsPerPixel int) byte {
	used := uint64(width) * uint64(bitsPerPixel) % 8
	if used == 0 {
		return 0
	}
	return 0xff >> used
}

func hasPadding(row []byte, width uint32, bitsPerPixel int) bool {
	return row[len(row)-1]&unusedBits(width, bitsPerPixel) != 0
}

// zeroPadding clears the unused bits of a row and reports whether any were set.
func zeroPadding(row []byte, width uint32, bitsPerPixel int) bool {
	mask := unusedBits(width, bitsPerPixel)
	last := &row[len(row)-1]
	if *last&mask == 0 {
		return false
	}
	*last &^= mask
	return true
}

// fixIndexes counts the palette indexes in row that are out of range, other
// than the value paletteLen itself. Viewers render every out-of-range index
// as opaque black, so the actual values are invisible and can carry data. If
// fix is set, it rewrites them to paletteLen, which renders the same.
func fixIndexes(row []byte, width uint32, depth uint8, paletteLen int, fix bool) int64 {
	var n int64
	perByte := 8 / int(depth)
	mask := byte(1<<depth - 1)
	for i := range int(width) {
		shift := uint(8 - int(depth)*(i%perByte+1))
		b := &row[i/perByte]
		if int(*b>>shift&mask) > paletteLen {
			n++
			if fix {
				*b = *b&^(mask<<shift) | byte(paletteLen)<<shift
			}
		}
	}
	return n
}

// unfilter reverses the filter ft applied to row, in place. prev is the
// previous unfiltered row (zeros for the first row of a pass).
func unfilter(ft byte, row, prev []byte, off int) error {
	switch ft {
	case 0:
	case 1:
		for i := off; i < len(row); i++ {
			row[i] += row[i-off]
		}
	case 2:
		for i := range row {
			row[i] += prev[i]
		}
	case 3:
		for i := range row {
			var left byte
			if i >= off {
				left = row[i-off]
			}
			row[i] += byte((int(left) + int(prev[i])) / 2)
		}
	case 4:
		for i := range row {
			var left, upLeft byte
			if i >= off {
				left, upLeft = row[i-off], prev[i-off]
			}
			row[i] += paeth(left, prev[i], upLeft)
		}
	default:
		return malformed("invalid filter type %d", ft)
	}
	return nil
}

// applyFilter writes the filter byte ft and row filtered with ft into dst.
func applyFilter(ft int, dst, row, prev []byte, off int) {
	dst[0] = byte(ft)
	out := dst[1:]
	for i := range row {
		var left, upLeft byte
		if i >= off {
			left, upLeft = row[i-off], prev[i-off]
		}
		switch ft {
		case 0:
			out[i] = row[i]
		case 1:
			out[i] = row[i] - left
		case 2:
			out[i] = row[i] - prev[i]
		case 3:
			out[i] = row[i] - byte((int(left)+int(prev[i]))/2)
		case 4:
			out[i] = row[i] - paeth(left, prev[i], upLeft)
		}
	}
}

// chooseFilter applies all five filters and returns the one with the smallest
// sum of absolute values, the heuristic the PNG spec recommends.
func chooseFilter(filtered *[5][]byte, row, prev []byte, off int) int {
	best, bestSum := 0, math.MaxInt
	for ft := range filtered {
		dst := filtered[ft][:1+len(row)]
		applyFilter(ft, dst, row, prev, off)
		sum := 0
		for _, b := range dst[1:] {
			if b < 128 {
				sum += int(b)
			} else {
				sum += 256 - int(b)
			}
		}
		if sum < bestSum {
			best, bestSum = ft, sum
		}
	}
	return best
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	switch {
	case pa <= pb && pa <= pc:
		return a
	case pb <= pc:
		return b
	}
	return c
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
