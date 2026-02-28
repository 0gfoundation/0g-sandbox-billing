// cmd/verify/main.go — verifies a Solidity contract on the 0G explorer
// (chainscan-galileo.0g.ai) using the Etherscan-compatible API.
//
// Usage examples:
//
//   # SandboxServing impl (no constructor args)
//   go run ./cmd/verify/ --contract 0x... --source contracts/src/SandboxServing.sol \
//     --source-key src/SandboxServing.sol --contract-name src/SandboxServing.sol:SandboxServing
//
//   # UpgradeableBeacon (two address constructor args)
//   go run ./cmd/verify/ --contract 0x... --source contracts/src/proxy/UpgradeableBeacon.sol \
//     --source-key src/proxy/UpgradeableBeacon.sol \
//     --contract-name src/proxy/UpgradeableBeacon.sol:UpgradeableBeacon \
//     --constructor-args <abi-encoded-hex>
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// standardJSONInput builds the solc standard-JSON input for a single source file.
// sourceKey is the path used as the key in the sources map (must match the
// compiler's view, e.g. "src/SandboxServing.sol").
func standardJSONInput(sourceKey, sourceCode string) (string, error) {
	input := map[string]any{
		"language": "Solidity",
		"sources": map[string]any{
			sourceKey: map[string]any{
				"content": sourceCode,
			},
		},
		"settings": map[string]any{
			"optimizer": map[string]any{
				"enabled": true,
				"runs":    200,
			},
			"outputSelection": map[string]any{
				"*": map[string]any{
					"*": []string{"abi", "evm.bytecode", "evm.deployedBytecode"},
				},
			},
		},
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func main() {
	contractAddr := flag.String("contract",      "", "deployed contract address (required)")
	apiURL       := flag.String("api",           "https://chainscan-galileo.0g.ai/open/api", "Etherscan-compatible API URL")
	sourcePath   := flag.String("source",        "contracts/src/SandboxServing.sol", "Solidity source file path on disk")
	sourceKey    := flag.String("source-key",    "src/SandboxServing.sol", "source key in standard-JSON (compiler path)")
	contractName := flag.String("contract-name", "src/SandboxServing.sol:SandboxServing", "fully-qualified contract name")
	compilerVer  := flag.String("compiler",      "v0.8.24+commit.e11b9ed9", "solc compiler version")
	chainID      := flag.String("chain-id",      "16602", "chain ID")
	apiKey       := flag.String("apikey",        "00", "API key (dummy value)")
	constructorArgs := flag.String("constructor-args", "", "ABI-encoded constructor args (hex, no 0x); empty for no args")
	flag.Parse()

	if *contractAddr == "" {
		fmt.Fprintln(os.Stderr, "error: --contract is required")
		os.Exit(1)
	}

	addr := strings.ToLower(*contractAddr)
	if !strings.HasPrefix(addr, "0x") {
		addr = "0x" + addr
	}

	// ── read source ───────────────────────────────────────────────────────────
	src, err := os.ReadFile(*sourcePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read source: %v\n", err)
		os.Exit(1)
	}

	stdJSON, err := standardJSONInput(*sourceKey, string(src))
	if err != nil {
		fmt.Fprintf(os.Stderr, "build standard JSON: %v\n", err)
		os.Exit(1)
	}

	// ── POST to Etherscan-compatible API ──────────────────────────────────────
	fmt.Printf("Contract      : %s\n", *contractAddr)
	fmt.Printf("Contract name : %s\n", *contractName)
	fmt.Printf("API URL       : %s\n", *apiURL)
	fmt.Printf("Compiler      : %s\n", *compilerVer)
	fmt.Printf("Submitting verification request...\n\n")

	form := url.Values{}
	form.Set("module",          "contract")
	form.Set("action",          "verifysourcecode")
	form.Set("apikey",          *apiKey)
	form.Set("chainid",         *chainID)
	form.Set("contractaddress", addr)
	form.Set("codeformat",      "solidity-standard-json-input")
	form.Set("sourceCode",      stdJSON)
	form.Set("contractname",    *contractName)
	form.Set("compilerversion", *compilerVer)
	form.Set("optimizationUsed", "1")
	form.Set("runs",            "200")
	// Etherscan API uses a typo ("Arguements") — intentional
	form.Set("constructorArguements", *constructorArgs)

	req, err := http.NewRequest(http.MethodPost, *apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "build request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept",       "application/json")

	httpClient := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "POST: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("HTTP status : %d\n", resp.StatusCode)

	var pretty map[string]any
	if json.Unmarshal(body, &pretty) == nil {
		out, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Println(string(body))
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "\n✗ Verification failed (HTTP %d)\n", resp.StatusCode)
		os.Exit(1)
	}

	var result struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err == nil {
		lower := strings.ToLower(result.Result + result.Message)
		if result.Status == "1" {
			fmt.Printf("\n✓ Verification submitted (GUID: %s)\n", result.Result)
			fmt.Printf("  Poll: curl '%s?module=contract&action=checkverifystatus&guid=%s&apikey=%s'\n",
				*apiURL, result.Result, *apiKey)
			fmt.Printf("  View: https://chainscan-galileo.0g.ai/address/%s#code\n", addr)
		} else if strings.Contains(lower, "already") {
			fmt.Printf("\n✓ Contract already verified.\n")
			fmt.Printf("  View: https://chainscan-galileo.0g.ai/address/%s#code\n", addr)
		} else {
			fmt.Fprintf(os.Stderr, "\n✗ Verification failed: [%s] %s\n", result.Status, result.Result)
			os.Exit(1)
		}
	}
}
