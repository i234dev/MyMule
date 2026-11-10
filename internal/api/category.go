package api

import (
	"errors"
	"net/http"

	"mymule/internal/ed2k"
	"mymule/internal/library"
)

// Categories: downloads that are not movies or episodes can be sent, when
// they finish, to a folder the user named in the preferences.

var errNoCategories = errors.New("categories are not available")

// category checks that name is a usable category and returns its name as
// saved ("" for none).
func (r *REST) category(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if r.prefs == nil || r.items == nil {
		return "", errNoCategories
	}
	c, ok := r.prefs.Category(name)
	if !ok {
		return "", errors.New("no category called " + name + " (or its folder is no longer accepted)")
	}
	return c.Name, nil
}

// setItemCategory sends a download to a category ("" for none). A movie or
// episode keeps its own folder.
func (r *REST) setItemCategory(h ed2k.Hash, name string) error {
	key := h.String()
	it, has := r.items.Get(key)
	if has && it.Kind != library.KindCategory {
		return errors.New("it is a movie or episode: it goes to its own folder")
	}
	switch {
	case name != "":
		r.items.Put(key, library.Item{Kind: library.KindCategory, Category: name})
	case has:
		r.items.Take(key)
	}
	return nil
}

// withCategory adds what the library knows to a listed download.
func (r *REST) withCategory(d RDownload) RDownload {
	if r.items == nil {
		return d
	}
	if it, ok := r.items.Get(d.Hash); ok {
		if it.Kind == library.KindCategory {
			d.Category = it.Category
		} else {
			d.Media = true
		}
	}
	return d
}

// setCategory: {"category": "Music"}, or "" for none. Finished downloads
// have already been moved, so they cannot change.
func (r *REST) setCategory(w http.ResponseWriter, req *http.Request) {
	f, ok := r.find(w, req)
	if !ok {
		return
	}
	var body struct {
		Category string `json:"category"`
	}
	if !decode(w, req, &body) {
		return
	}
	if f.Finished {
		fail(w, http.StatusConflict, errors.New("it has finished already"))
		return
	}
	name, err := r.category(body.Category)
	if err == nil {
		err = r.setItemCategory(f.Hash, name)
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	reply(w, map[string]any{"ok": true, "category": name})
}
