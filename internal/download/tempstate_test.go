package download

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"

	"mymule/internal/ed2k"
)

// plant writes a state file as someone with write access to the
// temporary folder could, and returns its path.
func plant(t *testing.T, dir string, m partMeta) string {
	t.Helper()
	data, _ := json.Marshal(m)
	p := filepath.Join(dir, m.Hash+".part.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A download whose data file is a link to another file is refused: it
// would make MyMule write downloaded data into, or upload, that file.
func TestOpenRefusesLinkedDataFile(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "mymule.conf")
	os.WriteFile(secret, []byte("api_key = do-not-share"), 0o600)
	h := ed2k.Hash{0x42}
	meta := plant(t, dir, partMeta{Hash: h.String(), Name: "film.mkv", Size: 22, Done: [][2]int64{{0, 22}}, Verified: []bool{true}})
	data := strings.TrimSuffix(meta, ".json")

	if err := os.Symlink(secret, data); err != nil {
		t.Log("symbolic links not available here:", err)
	} else {
		if _, err := Open(meta); err == nil {
			t.Fatal("download with a symlinked data file opened")
		}
		os.Remove(data)
	}
	if runtime.GOOS != "windows" {
		if err := os.Link(secret, data); err != nil {
			t.Log("hard links not available here:", err)
		} else if _, err := Open(meta); err == nil {
			t.Fatal("download with a hard-linked data file opened")
		}
		os.Remove(data)
	}
	// The state file itself as a link.
	os.Remove(meta)
	other := plant(t, t.TempDir(), partMeta{Hash: h.String(), Name: "film.mkv", Size: 22})
	if err := os.Symlink(other, meta); err == nil {
		if _, err := Open(meta); err == nil {
			t.Fatal("symlinked state file opened")
		}
	}
	if got, _ := os.ReadFile(secret); string(got) != "api_key = do-not-share" {
		t.Fatalf("the linked file was changed: %q", got)
	}
}

// Parts the state file claims verified are hashed before their data is
// uploaded; a forged claim is dropped and the part downloaded again.
func TestForgedVerifiedPartNotServed(t *testing.T) {
	dir := t.TempDir()
	good := testData(5000)
	link, _ := linkFor(t, "film.mkv", good)
	meta := plant(t, dir, partMeta{Hash: link.Hash.String(), Name: link.Name, Size: link.Size,
		Done: [][2]int64{{0, link.Size}}, Verified: []bool{true}})
	os.WriteFile(strings.TrimSuffix(meta, ".json"), make([]byte, len(good)), 0o600) // not the data
	pf, err := Open(meta)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	buf := make([]byte, 100)
	if _, err := pf.ReadVerified(buf, 0); err == nil {
		t.Fatal("forged verified part served")
	}
	if pf.VerifiedCount() != 0 || pf.Complete() {
		t.Fatal("forged part still counted as verified and done")
	}

	// The real data, verified per the state file, is served after its check.
	meta2 := plant(t, t.TempDir(), partMeta{Hash: link.Hash.String(), Name: link.Name, Size: link.Size,
		Done: [][2]int64{{0, link.Size}}, Verified: []bool{true}})
	os.WriteFile(strings.TrimSuffix(meta2, ".json"), good, 0o600)
	pf2, err := Open(meta2)
	if err != nil {
		t.Fatal(err)
	}
	defer pf2.Close()
	if _, err := pf2.ReadVerified(buf, 0); err != nil || string(buf) != string(good[:100]) {
		t.Fatalf("real verified part not served: %v", err)
	}
}

// A hash set in the state file must belong to the file: a wrong one would
// mark bad parts as good, and a short one used to crash MyMule.
func TestOpenRefusesWrongHashSet(t *testing.T) {
	size := int64(ed2k.PartSize)*2 + 10
	for name, set := range map[string][]string{
		"short": {ed2k.Hash{1}.String()},
		"wrong": {ed2k.Hash{1}.String(), ed2k.Hash{2}.String(), ed2k.Hash{3}.String()},
	} {
		meta := plant(t, t.TempDir(), partMeta{Hash: ed2k.Hash{9}.String(), Name: "x", Size: size, HashSet: set,
			Verified: []bool{true, true, true}})
		if pf, err := Open(meta); err == nil {
			pf.Close()
			t.Errorf("%s hash set accepted", name)
		}
	}
}

// What Finish moves into the downloads folder must be the file it just
// checked, not something put under the same name meanwhile.
func TestFinishRefusesSwappedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("an open file cannot be replaced on Windows")
	}
	dir, out := t.TempDir(), t.TempDir()
	data := testData(3000)
	link, _ := linkFor(t, "film.mkv", data)
	pf, err := create(dir, link, false)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	os.Rename(pf.dataPath, pf.dataPath+".old")
	os.WriteFile(pf.dataPath, []byte("something else"), 0o600)
	if _, err := pf.Finish(out); err == nil {
		t.Fatal("a swapped data file was finished")
	}
	if ents, _ := os.ReadDir(out); len(ents) != 0 {
		t.Fatalf("files in the downloads folder: %v", ents)
	}
}

// Names never end in a dot or a space of any kind (Windows drops them, so
// the file would get another name than the one given). Found by fuzzing.
func TestSanitizeNameNoTrailingDots(t *testing.T) {
	for _, in := range []string{"0. ", "film.　.　", "a. . .", "x ."} {
		out := SanitizeName(in)
		if strings.HasSuffix(out, ".") || strings.TrimRightFunc(out, unicode.IsSpace) != out {
			t.Errorf("SanitizeName(%q) = %q", in, out)
		}
	}
}
