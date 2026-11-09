package download

import (
	"mymule/internal/ed2k"
	"mymule/internal/share"
)

// sharedPart exposes a partial download to other peers through the share
// library. Only verified parts are announced and served.
type sharedPart struct{ pf *PartFile }

var _ share.File = sharedPart{}

func (s sharedPart) FileHash() ed2k.Hash { return s.pf.Hash }
func (s sharedPart) FileName() string    { return s.pf.Name }
func (s sharedPart) FileSize() int64     { return s.pf.Size }
func (s sharedPart) Partial() bool       { return true }
func (s sharedPart) Avail() []bool       { return s.pf.VerifiedParts() }

func (s sharedPart) PartHashes() []ed2k.Hash {
	// Below one part the file hash is the part's hash, and there is no set;
	// at exactly one part (and above) there is, and peers need it.
	if s.pf.Size < ed2k.PartSize {
		return nil
	}
	return s.pf.HashSet()
}

func (s sharedPart) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.pf.ReadVerified(p, off)
	if err != nil && n < len(p) {
		return n, share.ErrNotAvailable
	}
	return n, err
}
