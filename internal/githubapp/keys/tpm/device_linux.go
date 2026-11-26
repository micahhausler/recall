//go:build linux

package tpm

import (
	"fmt"

	"github.com/google/go-tpm/tpm2/transport"
	"github.com/google/go-tpm/tpm2/transport/linuxtpm"
)

// Device is the kernel's TPM resource manager, which lets processes share
// the TPM. Reaching it takes membership in the group that owns it,
// usually tss.
const Device = "/dev/tpmrm0"

// openDevice opens this machine's TPM.
func openDevice() (transport.TPMCloser, error) {
	t, err := linuxtpm.Open(Device)
	if err != nil {
		return nil, fmt.Errorf("opening the TPM: %w (a tpm: key needs a TPM 2.0 and access to %s, usually through the tss group)", err, Device)
	}
	return t, nil
}
