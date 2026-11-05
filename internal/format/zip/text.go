package zip

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// maxValueLen caps the length of a reported value.
	maxValueLen = 4096

	// maxReportName caps an entry name inside a field name or an error.
	maxReportName = 256
)

// hostUnix is the "version made by" host whose external attributes hold a
// Unix mode in their high 16 bits.
const hostUnix = 3

// hosts names the "version made by" hosts tools still write (APPNOTE 4.4.2).
var hosts = map[uint16]string{
	0:  "MS-DOS or Windows",
	3:  "Unix",
	7:  "Macintosh",
	10: "Windows NTFS",
	14: "VFAT",
	19: "OS X",
}

// archiver describes a "version made by" field: it tells which system and
// often which tool wrote the archive.
func archiver(madeBy uint16) string {
	host, ok := hosts[madeBy>>8]
	if !ok {
		host = fmt.Sprintf("host %d", madeBy>>8)
	}
	v := madeBy & 0xff
	return fmt.Sprintf("%s, ZIP %d.%d", host, v/10, v%10)
}

// timeRange collects the DOS dates of the entries.
type timeRange struct {
	n        int
	min, max uint32 // date<<16 | time, which sorts by time
}

func (tr *timeRange) add(date, t uint16) {
	v := uint32(date)<<16 | uint32(t)
	if tr.n == 0 || v < tr.min {
		tr.min = v
	}
	if tr.n == 0 || v > tr.max {
		tr.max = v
	}
	tr.n++
}

func (tr *timeRange) String() string {
	if tr.min == tr.max {
		return fmt.Sprintf("%d entries, %s", tr.n, dosString(tr.min))
	}
	return fmt.Sprintf("%d entries, from %s to %s", tr.n, dosString(tr.min), dosString(tr.max))
}

// dosString formats a DOS date and time (APPNOTE 4.4.6) as they are stored,
// in the local time of the system that wrote them.
func dosString(v uint32) string {
	date, t := v>>16, v&0xffff
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d",
		1980+date>>9, date>>5&15, date&31, t>>11, t>>5&63, t&31*2)
}

// dosTime handles an entry's date. The date Strip writes, and no date at
// all, say nothing.
func (p *processor) dosTime(date, t uint16) error {
	if date == dosDate && t == dosTime || date == 0 && t == 0 {
		return nil
	}
	if p.mode == modeVerify {
		return p.found("Entry date", dosString(uint32(date)<<16|uint32(t)))
	}
	p.time.add(date, t)
	return nil
}

// Extra field IDs (APPNOTE 4.5 and 4.6).
const (
	extraZIP64       = 0x0001
	extraNTFS        = 0x000a
	extraAES         = 0x9901
	extraTime        = 0x5455 // extended timestamp
	extraUnixOld     = 0x5855 // Info-ZIP Unix, old
	extraUnicodeCmt  = 0x6375
	extraUnicodePath = 0x7075
	extraUnixNew     = 0x7855 // Info-ZIP Unix, uid and gid
	extraUnix        = 0x7875 // Info-ZIP Unix, any size uid and gid
)

// maxExtraIDs caps the extra field IDs remembered per entry, so the central
// directory doesn't report them a second time.
const maxExtraIDs = 8

// extras handles the extra field of an entry's local header or central
// directory record. Strip drops them all; the ones extractors could read
// instead of the header fields are refused.
func (p *processor) extras(e *entry, b []byte, local bool) error {
	name := e.name
	for len(b) > 0 {
		if len(b) < 4 {
			return p.group("Extra field data", name, int64(len(b)))
		}
		id, size := binary.LittleEndian.Uint16(b), int(binary.LittleEndian.Uint16(b[2:]))
		if size > len(b)-4 {
			return p.group("Extra field data", name, int64(len(b)))
		}
		data := b[4 : 4+size]
		b = b[4+size:]
		if local && len(e.extraIDs) < maxExtraIDs {
			e.extraIDs = append(e.extraIDs, id)
		}
		report := local || !slices.Contains(e.extraIDs, id)

		var err error
		switch {
		case id == extraZIP64:
			return unsupported("ZIP64 extra field in %s", quote(name))
		case id == extraAES:
			return unsupported("encrypted entry %s", quote(name))
		case id == extraUnicodePath:
			// version 1, CRC of the header name, then the name in UTF-8
			if len(data) < 5 || data[0] != 1 || string(data[5:]) != name ||
				binary.LittleEndian.Uint32(data[1:]) != crc32.ChecksumIEEE([]byte(name)) {
				return unsupported("Unicode path extra field in %s that differs from its name", quote(name))
			}
			if report {
				err = p.group("Unicode path (extra field)", name, int64(size))
			}
		case !report:
		case id == extraTime:
			err = p.group("Extended timestamps (extra field)", name, int64(size))
		case id == extraNTFS:
			err = p.group("NTFS timestamps (extra field)", name, int64(size))
		case id == extraUnicodeCmt:
			err = p.group("Unicode comment (extra field)", name, int64(size))
		case id == extraUnix || id == extraUnixNew || id == extraUnixOld:
			err = p.group(owner(id, data), name, int64(size))
		default:
			err = p.group(fmt.Sprintf("Extra field %#04x", id), name, int64(size))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// owner describes the Unix user and group IDs in an extra field.
func owner(id uint16, data []byte) string {
	const what = "Unix owner (extra field)"
	switch id {
	case extraUnix:
		// version 1, then the uid and gid, each with its size first
		if len(data) < 2 || data[0] != 1 {
			return what
		}
		uid, rest, ok := sized(data[1:])
		if !ok {
			return what
		}
		gid, _, ok := sized(rest)
		if !ok {
			return what
		}
		return fmt.Sprintf("%s: uid %d, gid %d", what, uid, gid)
	case extraUnixNew:
		if len(data) == 4 {
			return fmt.Sprintf("%s: uid %d, gid %d", what, binary.LittleEndian.Uint16(data), binary.LittleEndian.Uint16(data[2:]))
		}
	case extraUnixOld:
		// access and modification times, then the uid and gid in local headers
		if len(data) >= 12 {
			return fmt.Sprintf("%s: uid %d, gid %d", what, binary.LittleEndian.Uint16(data[8:]), binary.LittleEndian.Uint16(data[10:]))
		}
	}
	return what
}

// sized reads a little-endian number of up to 8 bytes after its size.
func sized(b []byte) (v uint64, rest []byte, ok bool) {
	if len(b) < 1 || int(b[0]) > len(b)-1 || b[0] > 8 {
		return 0, nil, false
	}
	n := int(b[0])
	for i := n; i > 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v, b[1+n:], true
}

// text decodes b as UTF-8, or as Latin-1 if it isn't valid UTF-8, and clips
// it to maxValueLen bytes.
func text(b []byte) string {
	valid := utf8.Valid(b)
	b = b[:min(len(b), maxValueLen+1)]
	s := string(b)
	if !valid {
		var sb strings.Builder
		for _, c := range b {
			sb.WriteRune(rune(c))
		}
		s = sb.String()
	}
	return clip(s, maxValueLen)
}

// clip cuts s to n bytes and makes it valid UTF-8.
func clip(s string, n int) string {
	if len(s) > n {
		s = s[:n] + "… (truncated)"
	}
	return strings.ToValidUTF8(s, "�")
}

// quote formats an entry name for a message.
func quote(name string) string {
	return fmt.Sprintf("%q", clip(name, maxReportName))
}
