package server

import (
	"reflect"
	"testing"
)

// A search result keeps only small, fixed fields. Holding on to the tags
// a server sent let a few KB of compressed answer pin over 100 MB.
func TestResultKeepsNoTags(t *testing.T) {
	rt := reflect.TypeOf(Result{})
	for i := 0; i < rt.NumField(); i++ {
		switch f := rt.Field(i); f.Type.Kind() {
		case reflect.Slice, reflect.Map, reflect.Pointer, reflect.Interface:
			t.Errorf("Result.%s (%s) can hold whatever the server sent", f.Name, f.Type)
		}
	}
}
