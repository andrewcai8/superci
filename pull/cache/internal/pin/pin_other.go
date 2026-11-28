//go:build !linux

package pin

import (
	"errors"
	"os"
)

func mlock(*os.File, int64) (func(), error) {
	return nil, errors.New("pinning needs Linux (MAP_POPULATE)")
}
