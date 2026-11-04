package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mymule/internal/ed2k"
)

func cmdHash(args []string) error {
	fs := flag.NewFlagSet("hash", flag.ExitOnError)
	parts := fs.Bool("parts", false, "also print every part hash")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: mymule hash [-parts] <file>...")
	}
	for _, path := range fs.Args() {
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		total := st.Size()
		progress := func(done int64) {
			if total > 50<<20 {
				fmt.Fprintf(os.Stderr, "\rhashing %s: %3d%%", filepath.Base(path), done*100/total)
			}
		}
		res, err := ed2k.HashFile(path, progress)
		if total > 50<<20 {
			fmt.Fprintln(os.Stderr)
		}
		if err != nil {
			return err
		}
		link := ed2k.Link{Name: filepath.Base(path), Size: res.Size, Hash: res.Root}
		fmt.Printf("%s  %s\n%s\n", res.Root, path, link)
		if *parts {
			for i, p := range res.Parts {
				fmt.Printf("  part %3d: %s\n", i, p)
			}
		}
	}
	return nil
}
