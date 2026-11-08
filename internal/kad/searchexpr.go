package kad

import (
	"errors"
	"strings"

	"mymule/internal/proto"
)

// The search trees other nodes send with keyword searches (eMule's
// CreateSearchExpressionTree and SearchTermsMatch).

type termKind int

const (
	termAnd termKind = iota
	termOr
	termNot
	termString  // every word in the name
	termMeta    // a string tag equal to a value
	termEqual   // numeric comparisons
	termGreater //
	termLess
	termGreaterEq
	termLessEq
	termNotEqual
)

type term struct {
	kind        termKind
	left, right *term
	words       []string // termString
	tag         string   // tag name (one byte for ID tags)
	str         string   // termMeta value, lowercase
	num         uint64
}

// maxTreeDepth is eMule's limit on nested operators; maxTreeTerms, ours
// on the terms of a tree (eMule's own searches have a dozen at most): a
// large tree evaluated against every entry would cost us a lot of time.
const (
	maxTreeDepth = 24
	maxTreeTerms = 64  // terms in a tree
	maxTreeWords = 64  // words in all its strings
	maxTreeStr   = 255 // bytes of one string
)

var errBadTree = errors.New("kad: bad search tree")

func parseTree(r *proto.Reader, depth int) (*term, error) {
	left, words := maxTreeTerms, maxTreeWords
	return parseTerm(r, depth, &left, &words)
}

// treeStr reads a string of a search tree.
func treeStr(r *proto.Reader) (string, bool) {
	n := int(r.U16())
	if n > maxTreeStr {
		return "", false
	}
	b := r.Bytes(n)
	return strings.ToLower(text(b)), r.Err() == nil
}

func parseTerm(r *proto.Reader, depth int, left, words *int) (*term, error) {
	if depth >= maxTreeDepth || *left <= 0 {
		return nil, errBadTree
	}
	*left--
	op := r.U8()
	if r.Err() != nil {
		return nil, errBadTree
	}
	switch op {
	case seBool:
		var kind termKind
		switch r.U8() {
		case 0:
			kind = termAnd
		case 1:
			kind = termOr
		case 2:
			kind = termNot
		default:
			return nil, errBadTree
		}
		l, err := parseTerm(r, depth+1, left, words)
		if err != nil {
			return nil, err
		}
		rt, err := parseTerm(r, depth+1, left, words)
		if err != nil {
			return nil, err
		}
		return &term{kind: kind, left: l, right: rt}, nil
	case seString:
		s, ok := treeStr(r)
		if !ok {
			return nil, errBadTree
		}
		t := &term{kind: termString}
		for _, w := range strings.FieldsFunc(s, func(c rune) bool { return strings.ContainsRune(invalidKeywordChars, c) }) {
			if *words--; *words < 0 {
				return nil, errBadTree
			}
			t.words = append(t.words, w)
		}
		return t, nil
	case seMetaStr:
		v, ok := treeStr(r)
		name, ok2 := treeStr(r)
		if !ok || !ok2 {
			return nil, errBadTree
		}
		return &term{kind: termMeta, str: v, tag: name}, nil
	case seNumeric, seNumeric64:
		var v uint64
		if op == seNumeric {
			v = uint64(r.U32())
		} else {
			v = r.U64()
		}
		cmp := r.U8()
		name, ok := treeStr(r)
		if !ok || cmp > 5 {
			return nil, errBadTree
		}
		kinds := [...]termKind{termEqual, termGreater, termLess, termGreaterEq, termLessEq, termNotEqual}
		return &term{kind: kinds[cmp], tag: name, num: v}, nil
	}
	return nil, errBadTree
}

// tagName is the name a tag goes by in search trees: its one-byte ID or
// its string name.
func tagName(t proto.Tag) string {
	if t.Name != "" {
		return t.Name
	}
	return string([]byte{t.ID})
}

// matches reports whether a keyword entry fits the tree.
func (t *term) matches(e *entry) bool {
	var work int
	return t.matchesWork(e, &work)
}

// matchesWork is matches, adding the work done to *work: one unit per
// term, plus one per 64 bytes of name.
func (t *term) matchesWork(e *entry, work *int) bool {
	*work += 1 + len(e.lname)/64
	switch t.kind {
	case termAnd:
		return t.left.matchesWork(e, work) && t.right.matchesWork(e, work)
	case termOr:
		return t.left.matchesWork(e, work) || t.right.matchesWork(e, work)
	case termNot:
		return t.left.matchesWork(e, work) && !t.right.matchesWork(e, work)
	case termString:
		if len(t.words) == 0 {
			return false
		}
		for _, w := range t.words {
			if !strings.Contains(e.lname, w) {
				return false
			}
		}
		return true
	case termMeta:
		if t.tag == string([]byte{TagFileFormat}) {
			name := e.lname
			if i := strings.LastIndexByte(name, '.'); i >= 0 {
				return strings.EqualFold(name[i+1:], t.str)
			}
			return false
		}
		for _, tg := range e.tags {
			if tg.Type == proto.TagString && tagName(tg) == t.tag {
				return strings.EqualFold(tg.Str, t.str)
			}
		}
		return false
	}
	// Numeric: the size is always known; other tags must be there.
	var v uint64
	found := false
	if t.tag == string([]byte{TagFileSize}) {
		v, found = e.size, true
	} else {
		for _, tg := range e.tags {
			if (tg.Type == proto.TagUint32 || tg.Type == proto.TagUint64) && tagName(tg) == t.tag {
				v, found = tg.Int, true
				break
			}
		}
	}
	if !found {
		return false
	}
	switch t.kind {
	case termEqual:
		return v == t.num
	case termGreater:
		return v > t.num
	case termLess:
		return v < t.num
	case termGreaterEq:
		return v >= t.num
	case termLessEq:
		return v <= t.num
	case termNotEqual:
		return v != t.num
	}
	return false
}
