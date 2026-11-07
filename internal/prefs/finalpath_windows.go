package prefs

import (
	"strings"
	"syscall"
	"unsafe"
)

var procGetFinalPathNameByHandleW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFinalPathNameByHandleW")

// finalPath returns the path Windows really opens for the existing p,
// following junctions (mount points) as well as symbolic links. Since Go
// 1.23 filepath.EvalSymlinks leaves junctions alone, so D:\lib being a
// junction to C:\Windows would otherwise pass the folder rules.
func finalPath(p string) (string, error) {
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	// FILE_FLAG_BACKUP_SEMANTICS opens folders; no access is asked for.
	h, err := syscall.CreateFile(name, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(h)
	buf := make([]uint16, 512)
	for {
		n, _, e := procGetFinalPathNameByHandleW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
		if n == 0 {
			return "", e
		}
		if int(n) < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]uint16, n+1)
	}
	s := syscall.UTF16ToString(buf)
	switch {
	case strings.HasPrefix(s, `\\?\UNC\`):
		s = `\\` + s[len(`\\?\UNC\`):]
	case strings.HasPrefix(s, `\\?\`):
		s = s[len(`\\?\`):]
	}
	return s, nil
}
