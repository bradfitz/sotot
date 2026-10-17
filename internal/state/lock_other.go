//go:build !unix

package state

import "os"

// LockDir creates dir. On this platform it doesn't prevent two daemons
// from sharing a state directory.
func LockDir(dir string) (release func(), err error) {
	return func() {}, os.MkdirAll(dir, 0o700)
}
