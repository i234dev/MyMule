package download

import "sort"

// Range is a half-open byte range [Start, End).
type Range struct {
	Start, End int64
}

func (r Range) Len() int64 { return r.End - r.Start }

// RangeSet is a sorted list of non-overlapping, non-adjacent ranges.
type RangeSet []Range

// Add merges r into the set.
func (s *RangeSet) Add(r Range) {
	if r.End <= r.Start {
		return
	}
	out := (*s)[:0:0]
	inserted := false
	for _, x := range *s {
		switch {
		case x.End < r.Start:
			out = append(out, x)
		case r.End < x.Start:
			if !inserted {
				out = append(out, r)
				inserted = true
			}
			out = append(out, x)
		default: // overlapping or adjacent: absorb
			r.Start = min(r.Start, x.Start)
			r.End = max(r.End, x.End)
		}
	}
	if !inserted {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	*s = out
}

// Remove subtracts r from the set.
func (s *RangeSet) Remove(r Range) {
	if r.End <= r.Start {
		return
	}
	out := (*s)[:0:0]
	for _, x := range *s {
		if x.End <= r.Start || x.Start >= r.End {
			out = append(out, x)
			continue
		}
		if x.Start < r.Start {
			out = append(out, Range{x.Start, r.Start})
		}
		if x.End > r.End {
			out = append(out, Range{r.End, x.End})
		}
	}
	*s = out
}

// Covered returns how many bytes of r are in the set.
func (s RangeSet) Covered(r Range) int64 {
	var n int64
	for _, x := range s {
		lo, hi := max(x.Start, r.Start), min(x.End, r.End)
		if hi > lo {
			n += hi - lo
		}
	}
	return n
}

// Contains reports whether r is entirely in the set.
func (s RangeSet) Contains(r Range) bool { return s.Covered(r) == r.Len() }

// Total returns the number of bytes in the set.
func (s RangeSet) Total() int64 {
	var n int64
	for _, x := range s {
		n += x.Len()
	}
	return n
}
