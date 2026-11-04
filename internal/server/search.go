package server

import (
	"fmt"
	"strings"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

// File types understood by servers in search requests.
const (
	TypeAny      = ""
	TypeAudio    = "Audio"
	TypeVideo    = "Video"
	TypeImage    = "Image"
	TypeProgram  = "Pro"
	TypeDocument = "Doc"
	TypeArchive  = "Arc"
	TypeCDImage  = "Iso"
)

// ParseFileType maps user-friendly names to server file types.
func ParseFileType(s string) (string, error) {
	switch strings.ToLower(s) {
	case "", "any", "all":
		return TypeAny, nil
	case "audio", "music":
		return TypeAudio, nil
	case "video":
		return TypeVideo, nil
	case "image", "picture":
		return TypeImage, nil
	case "program", "pro", "app":
		return TypeProgram, nil
	case "doc", "document":
		return TypeDocument, nil
	case "arc", "archive":
		return TypeArchive, nil
	case "iso", "cd", "cdimage":
		return TypeCDImage, nil
	}
	return "", fmt.Errorf("unknown file type %q", s)
}

// Query is a server search request.
type Query struct {
	Keywords  string
	Type      string // one of Type*
	MinSize   uint64 // bytes, 0 = no limit
	MaxSize   uint64 // bytes, 0 = no limit
	Extension string // e.g. "mkv"
}

// Search expression opcodes.
const (
	seOpBool    = 0x00
	seString    = 0x01
	seMetaStr   = 0x02
	seNumeric   = 0x03
	seNumeric64 = 0x08

	boolAnd = 0x00

	cmpGreaterEq = 0x03
	cmpLessEq    = 0x04
)

// encode builds the prefix-notation search tree: AND(term1, AND(term2, ...)).
func (q Query) encode() ([]byte, error) {
	var terms []func(w *proto.Writer)

	for _, word := range strings.Fields(q.Keywords) {
		word := word
		terms = append(terms, func(w *proto.Writer) {
			w.U8(seString)
			w.Str16(word)
		})
	}
	if len(terms) == 0 {
		return nil, fmt.Errorf("empty search")
	}
	if q.Type != "" {
		terms = append(terms, func(w *proto.Writer) {
			w.U8(seMetaStr)
			w.Str16(q.Type)
			w.U16(1)
			w.U8(proto.FTFileType)
		})
	}
	if q.Extension != "" {
		ext := strings.TrimPrefix(q.Extension, ".")
		terms = append(terms, func(w *proto.Writer) {
			w.U8(seMetaStr)
			w.Str16(ext)
			w.U16(1)
			w.U8(proto.FTFileFormat)
		})
	}
	sizeTerm := func(v uint64, cmp byte) func(w *proto.Writer) {
		return func(w *proto.Writer) {
			if v > 0xFFFFFFFF {
				w.U8(seNumeric64)
				w.U64(v)
			} else {
				w.U8(seNumeric)
				w.U32(uint32(v))
			}
			w.U8(cmp)
			w.U16(1)
			w.U8(proto.FTFileSize)
		}
	}
	if q.MinSize > 0 {
		terms = append(terms, sizeTerm(q.MinSize, cmpGreaterEq))
	}
	if q.MaxSize > 0 {
		terms = append(terms, sizeTerm(q.MaxSize, cmpLessEq))
	}

	var w proto.Writer
	for i, t := range terms {
		if i < len(terms)-1 {
			w.U8(seOpBool)
			w.U8(boolAnd)
		}
		t(&w)
	}
	return w.B, nil
}

// Result is one file returned by a server search.
type Result struct {
	Hash            ed2k.Hash
	Name            string
	Size            uint64
	Type            string
	Sources         uint32
	CompleteSources uint32
	Tags            proto.TagList
}

// Link returns the ed2k:// link for the result.
func (r Result) Link() ed2k.Link {
	return ed2k.Link{Name: r.Name, Size: int64(r.Size), Hash: r.Hash}
}

// fillResult sets the convenience fields of a result from its tags.
func fillResult(res *Result) {
	res.Name = res.Tags.Str(proto.FTFileName)
	res.Size = res.Tags.Int(proto.FTFileSize) | res.Tags.Int(proto.FTFileSizeHi)<<32
	res.Type = res.Tags.Str(proto.FTFileType)
	res.Sources = uint32(res.Tags.Int(proto.FTSources))
	res.CompleteSources = uint32(res.Tags.Int(proto.FTCompleteSrcs))
}

// parseSearchResults decodes OP_SEARCHRESULT. The trailing byte (if
// present) says whether more results can be requested.
func parseSearchResults(payload []byte) (results []Result, more bool, err error) {
	r := proto.NewReader(payload)
	n := int(r.U32())
	for i := 0; i < n; i++ {
		var res Result
		res.Hash = r.Hash()
		r.U32() // client ID
		r.U16() // client port
		ntags := int(r.U32())
		if r.Err() != nil {
			return results, false, r.Err()
		}
		tags, err := r.ReadTags(ntags)
		if err != nil {
			return results, false, err
		}
		res.Tags = tags
		fillResult(&res)
		results = append(results, res)
	}
	if r.Len() >= 1 {
		more = r.U8() != 0
	}
	return results, more, nil
}
