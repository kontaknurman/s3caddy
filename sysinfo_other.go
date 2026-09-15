//go:build !linux

package main

import "errors"

// The panel targets Linux; on anything else the Status page still renders,
// with the disk usage card explaining itself.
var statfsUsage = func(path string) (fsUsage, error) {
	return fsUsage{}, errors.New("pemakaian disk hanya tersedia di Linux")
}
