package jpeg

import (
	"bufio"
	"math/bits"
)

// DC coefficient limits. With 8-bit samples the DC coefficient of a block is
// at most 1024 in magnitude, and a quantization step of 1 keeps that range.
// Keeping it to [-1024, 1023] means every difference between two blocks fits
// in the 11 bits the standard DC tables can encode.
const (
	minDC = -1024
	maxDC = 1023

	// maxDCCategory and maxACCategory are the largest coefficient sizes, in
	// bits, that 8-bit images produce and the standard tables encode.
	maxDCCategory = 11
	maxACCategory = 10
)

// zigzag-ordered blocks: coefficients are kept in the order they are coded,
// since the image is never transformed back to pixels.
type block [64]int32

// bitReader reads the entropy-coded data of a scan. It reads one byte only
// when it needs another bit, so after the last block the bits left over are
// exactly the padding of the last byte.
type bitReader struct {
	r    *bufio.Reader
	bits uint32 // unread bits of the current byte, in the low n bits
	n    uint
}

func (br *bitReader) bit() (uint32, error) {
	if br.n == 0 {
		if err := br.fill(); err != nil {
			return 0, err
		}
	}
	br.n--
	return br.bits >> br.n & 1, nil
}

// fill reads the next data byte. A 0xFF data byte is followed by a stuffed
// zero; 0xFF followed by anything else is a marker, which means the data ran
// out in the middle of a block.
func (br *bitReader) fill() error {
	b, err := br.r.ReadByte()
	if err != nil {
		return readErr(err, "truncated image data")
	}
	if b == 0xff {
		c, err := br.r.ReadByte()
		if err != nil {
			return readErr(err, "truncated image data")
		}
		if c != 0 {
			return malformed("image data ends inside a block (%s)", markerName(c))
		}
	}
	br.bits, br.n = uint32(b), 8
	return nil
}

func (br *bitReader) receive(n uint8) (uint32, error) {
	var v uint32
	for range n {
		b, err := br.bit()
		if err != nil {
			return 0, err
		}
		v = v<<1 | b
	}
	return v, nil
}

// decode reads one Huffman-coded symbol.
func (br *bitReader) decode(d *decoder) (byte, error) {
	var code int32
	for l := 1; l <= maxCodeLen; l++ {
		b, err := br.bit()
		if err != nil {
			return 0, err
		}
		code = code<<1 | int32(b) //nolint:gosec // b is one bit
		if code <= d.maxCode[l] {
			return d.symbols[d.valPtr[l]+code], nil
		}
	}
	return 0, malformed("invalid Huffman code")
}

// extend reads an s-bit coefficient value: s bits with a leading 0 encode a
// negative number (ITU T.81 F.2.2.1).
func (br *bitReader) extend(s uint8) (int32, error) {
	v, err := br.receive(s)
	if err != nil || s == 0 {
		return 0, err
	}
	if v < 1<<(s-1) {
		return int32(v) - (1 << s) + 1, nil //nolint:gosec // v < 2^11
	}
	return int32(v), nil //nolint:gosec // v < 2^11
}

// end finishes a stretch of entropy-coded data: it checks the padding of the
// last byte, counts any bytes between the data and the next marker, and
// returns that marker. Encoders pad with 1 bits and write the marker right
// after the data, so anything else is hidden data.
func (br *bitReader) end(st *stats) (byte, error) {
	if br.bits&(1<<br.n-1) != 1<<br.n-1 {
		st.padding++
	}
	br.n = 0
	var extra, fill int64
	for {
		b, err := br.r.ReadByte()
		if err != nil {
			return 0, readErr(err, "truncated file (missing EOI)")
		}
		if b != 0xff {
			extra++
			continue
		}
		c := byte(0xff)
		for c == 0xff {
			if c, err = br.r.ReadByte(); err != nil {
				return 0, readErr(err, "truncated file (missing EOI)")
			}
			fill++
		}
		fill--
		if c == 0 {
			extra += 2 + fill
			fill = 0
			continue
		}
		st.extra += extra
		st.stray += fill
		return c, nil
	}
}

// decodeBlock reads one block. pred holds the DC value of the previous block
// of the component, since DC values are coded as differences.
func (br *bitReader) decodeBlock(dc, ac *decoder, pred *int32, blk *block) error {
	*blk = block{}
	s, err := br.decode(dc)
	if err != nil {
		return err
	}
	if s > maxDCCategory {
		return malformed("DC difference of %d bits", s)
	}
	diff, err := br.extend(s)
	if err != nil {
		return err
	}
	*pred += diff
	if *pred < minDC || *pred > maxDC {
		return malformed("DC coefficient %d out of range", *pred)
	}
	blk[0] = *pred

	for k := 1; k < 64; k++ {
		rs, err := br.decode(ac)
		if err != nil {
			return err
		}
		run, size := int(rs>>4), rs&15
		if size == 0 {
			switch run {
			case 0: // end of block: the rest is zero
				return nil
			case 15: // sixteen zeros
				k += 15
				if k > 63 {
					return malformed("AC run past the end of the block")
				}
				continue
			}
			return malformed("invalid AC symbol 0x%02x", rs)
		}
		if size > maxACCategory {
			return malformed("AC coefficient of %d bits", size)
		}
		k += run
		if k > 63 {
			return malformed("AC run past the end of the block")
		}
		if blk[k], err = br.extend(size); err != nil {
			return err
		}
	}
	return nil
}

// bitWriter writes entropy-coded data with byte stuffing. Write errors stick
// in the bufio.Writer and are returned by its Flush.
type bitWriter struct {
	w    *bufio.Writer
	bits uint32 // pending bits, in the low n bits
	n    uint
}

func (bw *bitWriter) write(v uint32, n uint8) {
	bw.bits = bw.bits<<n | v&(1<<n-1)
	bw.n += uint(n)
	for bw.n >= 8 {
		bw.n -= 8
		b := byte(bw.bits >> bw.n) //nolint:gosec // the next 8 bits, cut on purpose
		_ = bw.w.WriteByte(b)
		if b == 0xff {
			_ = bw.w.WriteByte(0)
		}
	}
}

// flush pads the last byte with 1 bits, as the standard asks.
func (bw *bitWriter) flush() {
	if bw.n > 0 {
		bw.write(1<<(8-bw.n)-1, uint8(8-bw.n))
	}
}

func (bw *bitWriter) symbol(e *encoder, s byte) {
	bw.write(uint32(e.code[s]), e.size[s])
}

// encodeBlock writes one block with encoders dc and ac. Every value it gets
// came from decodeBlock, so every symbol has a code in the standard tables.
func (bw *bitWriter) encodeBlock(dc, ac *encoder, pred *int32, blk *block) {
	diff := blk[0] - *pred
	*pred = blk[0]
	s := category(diff)
	bw.symbol(dc, s)
	bw.write(magnitude(diff, s), s)

	run := 0
	for k := 1; k < 64; k++ {
		v := blk[k]
		if v == 0 {
			run++
			continue
		}
		for run > 15 {
			bw.symbol(ac, 0xf0)
			run -= 16
		}
		s := category(v)
		bw.symbol(ac, byte(run<<4)|s) //nolint:gosec // run < 16
		bw.write(magnitude(v, s), s)
		run = 0
	}
	if run > 0 {
		bw.symbol(ac, 0x00)
	}
}

// category returns the number of bits needed for |v|.
func category(v int32) uint8 {
	if v < 0 {
		v = -v
	}
	return uint8(bits.Len32(uint32(v))) //nolint:gosec // v is a bounded coefficient
}

// magnitude returns the bits that encode v in s bits: negative values are
// stored as v - 1 in two's complement, cut to s bits.
func magnitude(v int32, s uint8) uint32 {
	if v < 0 {
		v--
	}
	return uint32(v) & (1<<s - 1) //nolint:gosec // only the low s bits are kept
}

// stats counts hidden data found in and around the image data.
type stats struct {
	stray   int64 // fill and garbage bytes before markers
	extra   int64 // bytes between the image data and the next marker
	padding int   // places where padding bits are not all 1
	dummy   int   // blocks outside the image whose content isn't the canonical filler
}
