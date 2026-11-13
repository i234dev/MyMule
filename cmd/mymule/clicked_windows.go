package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procShellExecuteW         = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
)

// startedByDoubleClick reports whether MyMule was started from Windows
// Explorer (a double-click on mymule.exe) rather than from a console:
// Windows then made a console window for it alone, so it is the only
// process attached to it. From cmd or PowerShell, the shell is there too.
func startedByDoubleClick() bool {
	var ids [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	return n == 1
}

// openBrowser opens url in the default browser, the way programs do on
// Windows (ShellExecute). Starting rundll32 for it would look like what
// malware does, and antivirus programs flag it.
func openBrowser(url string) error {
	verb, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	const swShowNormal = 1
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(target)), 0, 0, swShowNormal)
	if r <= 32 { // ShellExecute returns a value above 32 when it worked
		return fmt.Errorf("ShellExecute error %d", r)
	}
	return nil
}
