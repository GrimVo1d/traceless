package jpeg

const (
	// maxCoefBytes caps the coefficients held for images that can't be
	// streamed: progressive ones and those split over several scans. At 128
	// bytes per block that is about 2 million blocks, a 90-megapixel photo
	// with 4:2:0 chroma.
	maxCoefBytes = 256 << 20

	// maxScans caps the number of scans. Each scan walks every block of its
	// components, even when its data is a few bytes, so the count bounds the
	// work. libjpeg's default progressive script has 10 scans.
	maxScans = 100

	// maxAC is the largest magnitude of an AC coefficient.
	maxAC = 1<<maxACCategory - 1
)

// coefs holds the quantized DCT coefficients of a whole image, in zigzag
// order, while its scans are read.
type coefs struct {
	comps []coefGrid
}

// coefGrid holds the blocks of one component. Block rows are allocated when
// a scan first reaches them, so a file cut off early doesn't cost the whole
// buffer. A DC scan can reach every row for one bit per block, though, so
// maxCoefBytes is what bounds memory.
type coefGrid struct {
	across int // blocks per row
	rows   [][]int16
	ends   [][]uint8 // per block, 1 + the index of its last nonzero AC coefficient, 0 if none
}

// newCoefs sizes the buffer for f. Interleaved scans also code the blocks
// that pad the last MCU of each row and column, so with several components
// the grids cover whole MCUs.
func newCoefs(f *frame) (*coefs, error) {
	type size struct{ across, down int }
	sizes := make([]size, len(f.comps))
	total := 0
	for i, c := range f.comps {
		sizes[i] = size{c.bw, c.bh}
		if len(f.comps) > 1 {
			mx, my := f.mcus()
			sizes[i] = size{mx * c.h, my * c.v}
		}
		total += sizes[i].across * sizes[i].down
	}
	if total > maxCoefBytes/128 {
		return nil, unsupported("image of %d blocks needs %d MB for its coefficients (limit %d MB)", total, total>>13, maxCoefBytes>>20)
	}
	cs := &coefs{comps: make([]coefGrid, len(f.comps))}
	for i, s := range sizes {
		cs.comps[i] = coefGrid{across: s.across, rows: make([][]int16, s.down), ends: make([][]uint8, s.down)}
	}
	return cs, nil
}

// block returns the coefficients of block (bx, by) and the end of its
// nonzero AC coefficients, allocating its row.
func (g *coefGrid) block(bx, by int) ([]int16, *uint8) {
	row := g.rows[by]
	if row == nil {
		row = make([]int16, 64*g.across)
		g.rows[by], g.ends[by] = row, make([]uint8, g.across)
	}
	return row[64*bx : 64*bx+64 : 64*bx+64], &g.ends[by][bx]
}

// setEnd records that coefficient k of a block is nonzero.
func setEnd(end *uint8, k int) {
	*end = max(*end, uint8(k+1)) //nolint:gosec // k < 64
}

// peek returns the coefficients of block (bx, by), or nil if no scan reached
// its row.
func (g *coefGrid) peek(bx, by int) []int16 {
	if row := g.rows[by]; row != nil {
		return row[64*bx : 64*bx+64 : 64*bx+64]
	}
	return nil
}

// The decoding procedures of progressive scans, ITU T.81 G.1.2. Each codes
// one bit position al of some coefficients: a first scan their upper bits,
// a refinement scan the next lower bit. Values are range-checked as they are
// written, so every finished block can be encoded with the standard tables.

// decodeDCFirst reads the upper bits of a DC coefficient. pred holds the
// previous block's DC value, shifted right by al.
func (br *bitReader) decodeDCFirst(d *decoder, pred *int32, al byte, coef []int16) error {
	s, err := br.decode(d)
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
	v := *pred << al
	if v < minDC || v > maxDC {
		return malformed("DC coefficient %d out of range", v)
	}
	coef[0] = int16(v)
	return nil
}

// decodeDCRefine reads bit al of a DC coefficient. The bits below the first
// scan's position are zero, so setting one keeps the value in range.
func (br *bitReader) decodeDCRefine(al byte, coef []int16) error {
	b, err := br.bit()
	if err != nil {
		return err
	}
	if b != 0 {
		coef[0] |= 1 << al
	}
	return nil
}

// decodeACFirst reads the upper bits of coefficients ss to se. eobrun counts
// the blocks left in a run of blocks that have nothing more in this scan.
func (br *bitReader) decodeACFirst(d *decoder, ss, se, al byte, eobrun *int32, coef []int16, end *uint8) error {
	if *eobrun > 0 {
		*eobrun--
		return nil
	}
	for k := int(ss); k <= int(se); k++ {
		rs, err := br.decode(d)
		if err != nil {
			return err
		}
		r, s := int(rs>>4), rs&15
		if s == 0 {
			if r < 15 {
				// End of band, for this block and 2^r - 1 + (r bits) more.
				extra, err := br.receive(uint8(r))
				if err != nil {
					return err
				}
				*eobrun = 1<<r - 1 + int32(extra) //nolint:gosec // extra < 2^14
				return nil
			}
			k += 15 // sixteen zeros
			if k > int(se) {
				return malformed("AC run past the end of the band")
			}
			continue
		}
		if s > maxACCategory {
			return malformed("AC coefficient of %d bits", s)
		}
		k += r
		if k > int(se) {
			return malformed("AC run past the end of the band")
		}
		v, err := br.extend(s)
		if err != nil {
			return err
		}
		if v <<= al; v < -maxAC || v > maxAC {
			return malformed("AC coefficient %d out of range", v)
		}
		setEnd(end, k)
		coef[k] = int16(v)
	}
	return nil
}

// decodeACRefine reads bit al of coefficients ss to se. Coefficients that are
// already nonzero get one correction bit each; the coded symbols place the
// ones that become nonzero at this bit. end bounds the search for nonzero
// coefficients in a run of blocks with nothing new, which reads no input.
func (br *bitReader) decodeACRefine(d *decoder, ss, se, al byte, eobrun *int32, coef []int16, end *uint8) error {
	k, last := int(ss), int(se)
	for ; *eobrun == 0 && k <= last; k++ {
		rs, err := br.decode(d)
		if err != nil {
			return err
		}
		r, s := int(rs>>4), rs&15
		var v int32
		switch {
		case s == 1:
			b, err := br.bit()
			if err != nil {
				return err
			}
			v = -1 << al
			if b != 0 {
				v = 1 << al
			}
			if v < -maxAC || v > maxAC {
				return malformed("AC coefficient %d out of range", v)
			}
		case s != 0:
			return malformed("AC refinement symbol 0x%02x", rs)
		case r < 15:
			extra, err := br.receive(uint8(r))
			if err != nil {
				return err
			}
			*eobrun = 1<<r + int32(extra) //nolint:gosec // extra < 2^14
			k--                           // the rest of this block is refined below
			continue
		}
		// Pass r zero coefficients (ZRL: 15, then stop at the 16th), refining
		// the nonzero ones on the way.
		for ; k <= last; k++ {
			if coef[k] == 0 {
				if r == 0 {
					break
				}
				r--
			} else if err := br.refine(&coef[k], al); err != nil {
				return err
			}
		}
		if k > last {
			// Go rejects this, libjpeg doesn't.
			return malformed("AC run past the end of the band")
		}
		if v != 0 {
			setEnd(end, k)
			coef[k] = int16(v) //nolint:gosec // checked above
		}
	}
	if *eobrun > 0 {
		for ; k <= last && k < int(*end); k++ {
			if coef[k] != 0 {
				if err := br.refine(&coef[k], al); err != nil {
					return err
				}
			}
		}
		*eobrun--
	}
	return nil
}

// refine reads the correction bit al of a nonzero coefficient.
func (br *bitReader) refine(c *int16, al byte) error {
	b, err := br.bit()
	if err != nil || b == 0 || *c&(1<<al) != 0 {
		return err
	}
	v := int32(*c) + 1<<al
	if *c < 0 {
		v = int32(*c) - 1<<al
	}
	if v < -maxAC || v > maxAC {
		return malformed("AC coefficient %d out of range", v)
	}
	*c = int16(v)
	return nil
}
