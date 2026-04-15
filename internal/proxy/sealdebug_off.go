//go:build !sealdebug

package proxy

import "github.com/0gfoundation/0g-sandbox/internal/daytona"

// sealBlocksAccess returns true in production builds: sealed sandboxes block
// SSH and toolbox access to prevent any external inspection of the container.
func sealBlocksAccess(sb *daytona.Sandbox) bool {
	return IsSealedSandbox(sb)
}
