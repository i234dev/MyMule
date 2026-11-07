//go:build !unix

package config

import "os"

func ownedByMe(os.FileInfo) bool { return true }
