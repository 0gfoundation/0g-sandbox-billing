.PHONY: build test test-contracts docker-build up down abigen tidy

build:
	go build ./cmd/billing/

test:
	go test ./...

# Run Solidity tests via Docker (requires Docker)
test-contracts:
	docker run --rm \
		-v $(PWD)/contracts:/contracts \
		--entrypoint forge \
		ghcr.io/foundry-rs/foundry:latest \
		test --root /contracts -vv

# Compile contracts and extract ABIs
build-contracts:
	chmod -R 777 contracts
	docker run --rm \
		-v $(PWD)/contracts:/contracts \
		--entrypoint forge \
		ghcr.io/foundry-rs/foundry:latest \
		build --root /contracts
	cat contracts/out/SandboxServing.sol/SandboxServing.json | \
		python3 -c "import sys,json; d=json.load(sys.stdin); print(json.dumps(d['abi'], indent=2))" \
		> contracts/abi/SandboxServing.json
	cat contracts/out/UpgradeableBeacon.sol/UpgradeableBeacon.json | \
		python3 -c "import sys,json; d=json.load(sys.stdin); print(json.dumps(d['abi'], indent=2))" \
		> contracts/abi/UpgradeableBeacon.json
	cat contracts/out/BeaconProxy.sol/BeaconProxy.json | \
		python3 -c "import sys,json; d=json.load(sys.stdin); print(json.dumps(d['abi'], indent=2))" \
		> contracts/abi/BeaconProxy.json

tidy:
	go mod tidy

docker-build:
	docker build -t 0g-sandbox-billing .

up:
	docker compose up -d

down:
	docker compose down

# Generate Go bindings from ABI (requires abigen from go-ethereum toolchain)
abigen:
	$(shell go env GOPATH)/bin/abigen \
		--abi contracts/abi/SandboxServing.json \
		--pkg chain \
		--type SandboxServing \
		--out internal/chain/sandbox_serving.go
	$(shell go env GOPATH)/bin/abigen \
		--abi contracts/abi/UpgradeableBeacon.json \
		--pkg chain \
		--type UpgradeableBeacon \
		--out internal/chain/upgradeable_beacon.go
	@echo "Go bindings regenerated."
