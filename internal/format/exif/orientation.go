package exif

import "encoding/binary"

// tagOrientation says how a viewer must rotate or flip the image (1 to 8).
const tagOrientation = 0x0112

// Orientation returns the Orientation tag of IFD0, from 1 to 8, or 0 if the
// data has none or it is not a single SHORT in that range. Only IFD0 is read:
// that is where viewers look.
func Orientation(data []byte) int {
	if len(data) < 8 {
		return 0
	}
	var order binary.ByteOrder
	switch string(data[:4]) {
	case "II*\x00":
		order = binary.LittleEndian
	case "MM\x00*":
		order = binary.BigEndian
	default:
		return 0
	}
	off := uint64(order.Uint32(data[4:]))
	if off+2 > uint64(len(data)) {
		return 0
	}
	n := uint64(order.Uint16(data[off:]))
	if n > maxEntries || off+2+12*n > uint64(len(data)) {
		return 0
	}
	for i := range n {
		e := data[off+2+12*i:][:12]
		if order.Uint16(e) != tagOrientation {
			continue
		}
		if order.Uint16(e[2:]) != typeShort || order.Uint32(e[4:]) != 1 {
			return 0
		}
		if o := int(order.Uint16(e[8:])); o >= 1 && o <= 8 {
			return o
		}
		return 0
	}
	return 0
}

// OrientationOnly returns big-endian EXIF data whose only tag is Orientation
// o. Stripped files carry it so viewers still rotate the image; it is built
// from the value alone, so nothing else from the original EXIF survives.
func OrientationOnly(o uint16) []byte {
	b := []byte("MM\x00*\x00\x00\x00\x08") // header, IFD0 at offset 8
	b = binary.BigEndian.AppendUint16(b, 1)
	b = binary.BigEndian.AppendUint16(b, tagOrientation)
	b = binary.BigEndian.AppendUint16(b, typeShort)
	b = binary.BigEndian.AppendUint32(b, 1)
	b = binary.BigEndian.AppendUint16(b, o)
	b = append(b, 0, 0)                        // unused half of the value field
	return binary.BigEndian.AppendUint32(b, 0) // no next IFD
}
