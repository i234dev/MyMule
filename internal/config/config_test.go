package config

import "testing"

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
