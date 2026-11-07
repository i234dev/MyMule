//go:build !unix

package download

import "os"

const noFollow = 0

func extraLinks(os.FileInfo) bool { return false }
