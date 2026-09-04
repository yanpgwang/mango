//go:build !linux

package main

import "errors"

func protectProcessCredentials() error {
	return errors.New("protect item runner credentials: Linux is required")
}
