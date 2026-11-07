//go:build !unix

package safepath

import "io/fs"

// notPrivate checks nothing beyond the type: off Unix the permission bits Go
// reports are synthetic (a writable directory reads 0666) and carry no owner
// (review 0ca6 F1).
func notPrivate(fs.FileInfo) string { return "" }
