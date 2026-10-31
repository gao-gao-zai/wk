//go:build !linux

package lifecycle

import "errors"

func Lock(string) (func(), error) {
	return nil, errors.New("seamless upgrade requires Linux local file locking")
}
