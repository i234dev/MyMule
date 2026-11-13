package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"mymule/internal/config"
	"mymule/internal/prefs"
)

// Folders changed from the web interface follow the folder rules even when
// written inside the data folder: a link (junction) out of it and a hidden
// folder are refused and mymule.conf is left as it was; the usual folders
// still work.
func TestSettingsFoldersInsideDataFolder(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `Z:\no-such-appdata-for-tests`)
	t.Setenv("APPDATA", `Z:\no-such-appdata-for-tests`)
	home, outside := t.TempDir(), t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	conf := filepath.Join(home, config.SettingsFile)
	os.WriteFile(conf, []byte("# empty\n"), 0o600)
	ps := prefs.Open(filepath.Join(home, "preferences.json"), home)
	ps.AllowRoots(filepath.Join(home, "downloads"))

	escape := filepath.Join(home, "escape")
	var err error
	if runtime.GOOS == "windows" {
		err = exec.Command("cmd", "/c", "mklink", "/J", escape, outside).Run()
	} else {
		err = os.Symlink(outside, escape)
	}
	if err != nil {
		t.Skipf("cannot make a link here: %v", err)
	}
	before, _ := os.ReadFile(conf)
	for _, d := range []string{"escape", ".hidden-startup"} {
		if err := config.UpdateSettings(map[string]string{"download_dir": d}, ps.CheckRooted); err == nil {
			t.Errorf("download_dir = %s accepted", d)
		}
	}
	if after, _ := os.ReadFile(conf); string(after) != string(before) {
		t.Fatal("mymule.conf changed")
	}
	if err := config.UpdateSettings(map[string]string{"download_dir": "finished"}, ps.CheckRooted); err != nil {
		t.Fatalf("a usual folder in the data folder refused: %v", err)
	}
}
