package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A port another program holds is a problem; a free one is fine.
func TestDoctorPorts(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var d doctor
	d.portFree("tcp", l.Addr().(*net.TCPAddr).Port)
	if len(d.list) != 1 || d.list[0].level != 2 {
		t.Fatalf("busy port: %+v", d.list)
	}
	if plural(1, "problem") != "1 problem" || plural(2, "warning") != "2 warnings" {
		t.Fatal("plural")
	}
}

// A folder MyMule cannot write in is told, and checking leaves nothing.
func TestDoctorWritable(t *testing.T) {
	dir := t.TempDir()
	if err := writable(dir); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
	if writable(filepath.Join(dir, "missing")) == nil {
		t.Fatal("a missing folder is writable")
	}
}
