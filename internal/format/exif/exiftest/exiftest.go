// Package exiftest builds EXIF data for tests.
package exiftest

import (
	"encoding/binary"
	"slices"
)

// Entry is one tag. Value is a string (ASCII), []byte (UNDEFINED), []uint16
// (SHORT), []uint32 (LONG) or [][2]uint32 (RATIONAL).
type Entry struct {
	Tag   uint16
	Value any
}

// IFD is a directory. Sub maps pointer tags (0x8769 Exif, 0x8825 GPS,
// 0xA005 interoperability) to the IFDs they point to; Next is the next IFD
// in the chain (IFD1 after IFD0).
type IFD struct {
	Entries []Entry
	Sub     map[uint16]*IFD
	Next    *IFD
}

// Order is a byte order that can also append: binary.BigEndian or
// binary.LittleEndian.
type Order interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

// Build encodes ifd0 as TIFF-structured EXIF data in the given byte order.
func Build(order Order, ifd0 *IFD) []byte {
	b := []byte("MM\x00*")
	if order.String() == binary.LittleEndian.String() {
		b = []byte("II*\x00")
	}
	b = order.AppendUint32(b, 8)
	return write(order, b, ifd0)
}

type raw struct {
	tag, typ uint16
	count    uint32
	data     []byte
	sub      *IFD
}

func write(o Order, b []byte, ifd *IFD) []byte {
	var entries []raw
	for _, e := range ifd.Entries {
		entries = append(entries, encode(o, e))
	}
	for tag, sub := range ifd.Sub {
		entries = append(entries, raw{tag: tag, typ: 4, count: 1, data: make([]byte, 4), sub: sub})
	}
	slices.SortFunc(entries, func(a, b raw) int { return int(a.tag) - int(b.tag) })

	table := len(b)
	b = o.AppendUint16(b, uint16(len(entries)))
	b = append(b, make([]byte, 12*len(entries)+4)...)
	for i, e := range entries {
		pos := table + 2 + 12*i
		o.PutUint16(b[pos:], e.tag)
		o.PutUint16(b[pos+2:], e.typ)
		o.PutUint32(b[pos+4:], e.count)
		if len(e.data) <= 4 {
			copy(b[pos+8:], e.data)
			continue
		}
		o.PutUint32(b[pos+8:], uint32(len(b)))
		b = append(b, e.data...)
		if len(b)%2 == 1 {
			b = append(b, 0)
		}
	}
	for i, e := range entries {
		if e.sub != nil {
			o.PutUint32(b[table+2+12*i+8:], uint32(len(b)))
			b = write(o, b, e.sub)
		}
	}
	if ifd.Next != nil {
		o.PutUint32(b[table+2+12*len(entries):], uint32(len(b)))
		b = write(o, b, ifd.Next)
	}
	return b
}

func encode(o Order, e Entry) raw {
	r := raw{tag: e.Tag}
	switch v := e.Value.(type) {
	case string:
		r.typ, r.count, r.data = 2, uint32(len(v)+1), append([]byte(v), 0)
	case []byte:
		r.typ, r.count, r.data = 7, uint32(len(v)), v
	case []uint16:
		r.typ, r.count = 3, uint32(len(v))
		for _, x := range v {
			r.data = o.AppendUint16(r.data, x)
		}
	case []uint32:
		r.typ, r.count = 4, uint32(len(v))
		for _, x := range v {
			r.data = o.AppendUint32(r.data, x)
		}
	case [][2]uint32:
		r.typ, r.count = 5, uint32(len(v))
		for _, x := range v {
			r.data = o.AppendUint32(o.AppendUint32(r.data, x[0]), x[1])
		}
	default:
		panic("exiftest: unsupported value type")
	}
	return r
}

// Photo returns EXIF data like a phone camera writes: make, model, dates,
// owner, serial number, a GPS position (Berlin, 52.52 N 13.405 E) and a
// thumbnail entry. All values are made up.
func Photo(order Order) []byte {
	return Build(order, &IFD{
		Entries: []Entry{
			{0x010f, "Google"},
			{0x0110, "Pixel 7 Pro"},
			{0x0112, []uint16{6}},
			{0x0131, "HDR+ 1.0.540104767zd"},
			{0x0132, "2024:05:01 12:30:00"},
		},
		Sub: map[uint16]*IFD{
			0x8769: {Entries: []Entry{
				{0x829a, [][2]uint32{{1, 125}}},
				{0x829d, [][2]uint32{{185, 100}}},
				{0x8827, []uint16{100}},
				{0x9003, "2024:05:01 12:30:00"},
				{0x9286, []byte("ASCII\x00\x00\x00taken at home")},
				{0xa430, "Jane Doe"},
				{0xa431, "SN-1234567"},
			}},
			0x8825: {Entries: []Entry{
				{0x01, "N"},
				{0x02, [][2]uint32{{52, 1}, {31, 1}, {1200, 100}}},
				{0x03, "E"},
				{0x04, [][2]uint32{{13, 1}, {24, 1}, {1800, 100}}},
				{0x06, [][2]uint32{{34, 1}}},
			}},
		},
		Next: &IFD{Entries: []Entry{
			{0x0103, []uint16{6}},
			{0x0201, []uint32{0}},
			{0x0202, []uint32{5120}},
		}},
	})
}
