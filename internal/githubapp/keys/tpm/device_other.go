//go:build !linux

package tpm

import (
	"errors"

	"github.com/google/go-tpm/tpm2/transport"
)

// openDevice has no TPM to open: tpm: keys are supported on Linux only.
func openDevice() (transport.TPMCloser, error) {
	return nil, errors.New("tpm: keys are supported on Linux only")
}
