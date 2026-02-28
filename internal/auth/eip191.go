package auth

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// HashMessage constructs the EIP-191 prefixed hash:
// keccak256("\x19Ethereum Signed Message:\n" + len(msg) + msg)
func HashMessage(msg []byte) []byte {
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(msg))
	return crypto.Keccak256([]byte(prefix), msg)
}

// Recover extracts the signer address from an EIP-191 signature.
// sig must be 65 bytes (R || S || V), with V in {0,1} or {27,28}.
func Recover(msg []byte, sig []byte) (common.Address, error) {
	if len(sig) != 65 {
		return common.Address{}, errors.New("invalid signature length")
	}
	hash := HashMessage(msg)

	// Normalize V: Ethereum uses 27/28, ecrecover expects 0/1
	sigCopy := make([]byte, 65)
	copy(sigCopy, sig)
	if sigCopy[64] >= 27 {
		sigCopy[64] -= 27
	}

	pub, err := crypto.SigToPub(hash, sigCopy)
	if err != nil {
		return common.Address{}, fmt.Errorf("ecrecover: %w", err)
	}
	return crypto.PubkeyToAddress(*pub), nil
}
