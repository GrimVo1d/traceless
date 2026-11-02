package format

import "fmt"

// MaxFields caps how many fields a Report returns, so a file with millions of
// metadata parts can't make Inspect use unbounded memory.
const MaxFields = 256

// Report collects Inspect fields. Repeated generic parts are grouped into one
// field, and fields past MaxFields are only counted.
type Report struct {
	// Unit names the grouped parts in a group's value, e.g. "chunks".
	Unit string

	fields  []Field
	groups  map[string]*group
	omitted int
}

type group struct {
	index int
	count int
	bytes int64
}

// Add records f. A grouped field is merged with earlier ones of the same name
// into "N <unit>, M bytes", where size is the part's size.
func (r *Report) Add(f Field, grouped bool, size int64) {
	if grouped {
		if g, ok := r.groups[f.Name]; ok {
			g.count++
			g.bytes += size
			r.fields[g.index].Value = fmt.Sprintf("%d %s, %d bytes", g.count, r.Unit, g.bytes)
			return
		}
	}
	if len(r.fields) >= MaxFields {
		r.omitted++
		return
	}
	if grouped {
		if r.groups == nil {
			r.groups = make(map[string]*group)
		}
		r.groups[f.Name] = &group{index: len(r.fields), count: 1, bytes: size}
	}
	r.fields = append(r.fields, f)
}

// Result returns the fields, with a final one counting those left out.
func (r *Report) Result() []Field {
	if r.omitted > 0 {
		return append(r.fields, Field{
			Name:  "More metadata",
			Value: fmt.Sprintf("%d further items not shown", r.omitted),
		})
	}
	return r.fields
}
