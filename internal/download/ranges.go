package download

import "sort"

// Range is a half-open byte range [Start, End).
type Range struct {
	Start, End int64
}

func (r Range) Len() int64 { return r.End - r.Start }

// RangeSet is a sorted list of non-overlapping, non-adjacent ranges.
//
// Lookups use binary search, so the cost of an operation grows with the
// ranges it touches, not with the size of the set: a peer sending many
// tiny scattered pieces used to make every write rebuild and re-sort the
// whole set, minutes of CPU with the download locked.
type RangeSet []Range

// first returns the index of the first range that ends at or after pos.
func (s RangeSet) first(pos int64) int {
	return sort.Search(len(s), func(k int) bool { return s[k].End >= pos })
}

// Add merges r into the set.
func (s *RangeSet) Add(r Range) {
	if r.End <= r.Start {
		return
	}
	rs := *s
	i := rs.first(r.Start)
	j := i
	for j < len(rs) && rs[j].Start <= r.End { // overlapping or adjacent: absorb
		r.Start = min(r.Start, rs[j].Start)
		r.End = max(r.End, rs[j].End)
		j++
	}
	if i == j {
		rs = append(rs, Range{})
		copy(rs[i+1:], rs[i:])
		rs[i] = r
	} else {
		rs[i] = r
		rs = append(rs[:i+1], rs[j:]...)
	}
	*s = rs
}

// Remove subtracts r from the set.
func (s *RangeSet) Remove(r Range) {
	if r.End <= r.Start {
		return
	}
	rs := *s
	i := rs.first(r.Start + 1) // ranges ending at r.Start are untouched
	j := i
	var keep []Range
	for j < len(rs) && rs[j].Start < r.End {
		x := rs[j]
		if x.Start < r.Start {
			keep = append(keep, Range{x.Start, r.Start})
		}
		if x.End > r.End {
			keep = append(keep, Range{r.End, x.End})
		}
		j++
	}
	if i == j {
		return
	}
	out := make(RangeSet, 0, len(rs)-(j-i)+len(keep))
	out = append(out, rs[:i]...)
	out = append(out, keep...)
	out = append(out, rs[j:]...)
	*s = out
}

// Covered returns how many bytes of r are in the set.
func (s RangeSet) Covered(r Range) int64 {
	var n int64
	for i := s.first(r.Start + 1); i < len(s) && s[i].Start < r.End; i++ {
		lo, hi := max(s[i].Start, r.Start), min(s[i].End, r.End)
		if hi > lo {
			n += hi - lo
		}
	}
	return n
}

// Contains reports whether r is entirely in the set.
func (s RangeSet) Contains(r Range) bool { return s.Covered(r) == r.Len() }

// Pieces returns how many ranges of the set overlap or touch r.
func (s RangeSet) Pieces(r Range) int {
	n := 0
	for i := s.first(r.Start); i < len(s) && s[i].Start <= r.End; i++ {
		n++
	}
	return n
}

// Total returns the number of bytes in the set.
func (s RangeSet) Total() int64 {
	var n int64
	for _, x := range s {
		n += x.Len()
	}
	return n
}
