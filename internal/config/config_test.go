package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLockIsExclusive(t *testing.T) {
	t.Setenv("MYMULE_HOME", t.TempDir())
	release, err := Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(); err == nil {
		t.Fatal("second lock succeeded while first is held")
	}
	release()
	release2, err := Lock()
	if err != nil {
		t.Fatalf("relock after release: %v", err)
	}
	release2()
}

func TestUserHashStable(t *testing.T) {
	t.Setenv("MYMULE_HOME", t.TempDir())
	a, err := UserHash()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := UserHash()
	if a != b || a[5] != 14 || a[14] != 111 {
		t.Fatalf("hash %s / %s", a, b)
	}
}

// The signing key comes from the api_key and a secret of our own, kept
// across starts: the same api_key gives the same key, another api_key
// another, and without the secret nobody can make it.
func TestSigningKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	a, err := SigningKey("the-api-key-0123456789")
	if err != nil || len(a) != 32 {
		t.Fatal(err)
	}
	b, _ := SigningKey("the-api-key-0123456789")
	c, _ := SigningKey("another-api-key-012345")
	if !bytes.Equal(a, b) || bytes.Equal(a, c) || bytes.Equal(a, []byte("the-api-key-0123456789")) {
		t.Fatal("not stable, or not tied to the api_key")
	}
	os.Remove(filepath.Join(home, "sign.key"))
	d, _ := SigningKey("the-api-key-0123456789")
	if bytes.Equal(a, d) {
		t.Fatal("the key does not depend on the secret")
	}
}
