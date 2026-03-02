package main

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/0gfoundation/0g-sandbox-billing/internal/chain"
)

func main() {
	eth, _ := ethclient.Dial("https://evmrpc-testnet.0g.ai")
	privKey, _ := crypto.HexToECDSA("859c3bd1baf85767059b81448d0902d2bb649d137f0df460eb576915d15d58eb")
	addr := crypto.PubkeyToAddress(privKey.PublicKey)
	c, _ := chain.NewSandboxServing(common.HexToAddress("0x24cD979DBd0Ae924a3f0c832a724CF4C58E5C210"), eth)
	opts := &bind.CallOpts{Context: context.Background()}
	acct, _ := c.GetAccount(opts, addr)
	nonce, _ := c.GetLastNonce(opts, addr, addr)
	earnings, _ := c.GetProviderEarnings(opts, addr)
	fmt.Printf("balance:   %s neuron\n", acct.Balance)
	fmt.Printf("nonce:     %s\n", nonce)
	fmt.Printf("earnings:  %s neuron\n", earnings)
}
