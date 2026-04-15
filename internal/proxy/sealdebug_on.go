//go:build sealdebug

package proxy

import "github.com/0gfoundation/0g-sandbox/internal/daytona"

// sealBlocksAccess always returns false in sealdebug builds: sealed sandboxes
// still receive TEE attestation but SSH and toolbox remain open for inspection.
func sealBlocksAccess(_ *daytona.Sandbox) bool {
	return false
}
