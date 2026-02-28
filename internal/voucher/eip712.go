package voucher

import (
	"crypto/ecdsa"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var voucherTypeHash = crypto.Keccak256Hash([]byte(
	"SandboxVoucher(address user,address provider,bytes32 usageHash,uint256 nonce,uint256 totalFee)",
))

// domainSeparator computes the EIP-712 domain separator.
func domainSeparator(chainID *big.Int, contractAddr common.Address) [32]byte {
	domainTypeHash := crypto.Keccak256Hash([]byte(
		"EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)",
	))
	nameHash := crypto.Keccak256Hash([]byte("0G Sandbox Serving"))
	versionHash := crypto.Keccak256Hash([]byte("1"))

	// ABI-encode: (bytes32, bytes32, bytes32, uint256, address)
	// Each element is padded to 32 bytes (left-padded for uint/addr, right-padded isn't used here)
	encoded := make([]byte, 5*32)
	copy(encoded[0:32], domainTypeHash[:])
	copy(encoded[32:64], nameHash[:])
	copy(encoded[64:96], versionHash[:])
	chainID.FillBytes(encoded[96:128])
	copy(encoded[140:160], contractAddr.Bytes()) // addr is right-aligned in 32-byte slot

	return crypto.Keccak256Hash(encoded)
}

// BuildUsageHash builds keccak256(sandboxID, periodStart, periodEnd, computeMinutes).
func BuildUsageHash(sandboxID string, periodStart, periodEnd, computeMinutes int64) [32]byte {
	data := make([]byte, 0, len(sandboxID)+8+8+8)
	data = append(data, []byte(sandboxID)...)
	data = appendInt64(data, periodStart)
	data = appendInt64(data, periodEnd)
	data = appendInt64(data, computeMinutes)
	return crypto.Keccak256Hash(data)
}

func appendInt64(b []byte, v int64) []byte {
	return append(b,
		byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v),
	)
}

// Verify recovers the signer address from a signed voucher.
// Useful for testing and on-chain pre-verification.
func Verify(v *SandboxVoucher, chainID *big.Int, contractAddr common.Address) (common.Address, error) {
	digest := hashVoucher(v, chainID, contractAddr)
	sig := make([]byte, 65)
	copy(sig, v.Signature)
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	pub, err := crypto.SigToPub(digest[:], sig)
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pub), nil
}

// Sign signs the voucher in-place with the TEE private key using EIP-712.
func Sign(v *SandboxVoucher, privKey *ecdsa.PrivateKey, chainID *big.Int, contractAddr common.Address) error {
	digest := hashVoucher(v, chainID, contractAddr)
	sig, err := crypto.Sign(digest[:], privKey)
	if err != nil {
		return err
	}
	// Convert V from 0/1 to 27/28 for Solidity ecrecover
	sig[64] += 27
	v.Signature = sig
	return nil
}

func hashVoucher(v *SandboxVoucher, chainID *big.Int, contractAddr common.Address) [32]byte {
	// structHash = keccak256(typeHash || abi.encode(fields))
	encoded := make([]byte, 6*32)
	copy(encoded[0:32], voucherTypeHash[:])
	copy(encoded[44:64], v.User.Bytes())    // padded address
	copy(encoded[76:96], v.Provider.Bytes())
	copy(encoded[96:128], v.UsageHash[:])
	v.Nonce.FillBytes(encoded[128:160])
	v.TotalFee.FillBytes(encoded[160:192])

	structHash := crypto.Keccak256Hash(encoded)
	sep := domainSeparator(chainID, contractAddr)

	// Final digest: keccak256(0x1901 || domainSeparator || structHash)
	msg := make([]byte, 2+32+32)
	msg[0] = 0x19
	msg[1] = 0x01
	copy(msg[2:34], sep[:])
	copy(msg[34:66], structHash[:])
	return crypto.Keccak256Hash(msg)
}
