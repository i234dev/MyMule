package config

import (
	"fmt"
	"os"
)

// Lock takes an exclusive lock on the data directory so that two mymule
// processes never run with the same identity (servers kick duplicate
// user hashes, and both would write the same .part files). The OS
// releases the lock automatically if the process dies.
func Lock() (release func(), err error) {
	p, err := Path("mymule.lock")
	if err != nil {
		return nil, err
	}
	f, err := lockFile(p)
	if err != nil {
		dir, _ := Dir()
		return nil, fmt.Errorf("another mymule is already running with data dir %s (set MYMULE_HOME to run a second instance)", dir)
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() { f.Close() }, nil
}
