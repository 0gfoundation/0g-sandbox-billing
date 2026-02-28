// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @dev Minimal interface used to query the beacon for the current implementation.
interface IBeacon {
    function implementation() external view returns (address);
}

/// @title BeaconProxy
/// @notice ERC-1967 proxy that reads its implementation address from a UpgradeableBeacon.
///         All ETH and state live in this contract; logic lives in the implementation.
///
///         Beacon slot (ERC-1967):
///           keccak256("eip1967.proxy.beacon") - 1
///           = 0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50
contract BeaconProxy {

    bytes32 private constant _BEACON_SLOT =
        0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50;

    /// @param beacon  Address of the UpgradeableBeacon.
    /// @param data    If non-empty, sent as a delegatecall to the implementation (runs initialize).
    constructor(address beacon, bytes memory data) payable {
        _setBeacon(beacon);
        if (data.length > 0) {
            address impl = _implementation();
            (bool success, bytes memory ret) = impl.delegatecall(data);
            if (!success) {
                // Bubble up the revert reason if available.
                if (ret.length > 0) {
                    assembly {
                        revert(add(ret, 32), mload(ret))
                    }
                }
                revert("BeaconProxy: init failed");
            }
        }
    }

    // ─── Internal helpers ─────────────────────────────────────────────────────

    function _setBeacon(address beacon) internal {
        require(beacon.code.length > 0, "BeaconProxy: not a contract");
        assembly {
            sstore(_BEACON_SLOT, beacon)
        }
    }

    function _beacon() internal view returns (address beacon) {
        assembly {
            beacon := sload(_BEACON_SLOT)
        }
    }

    function _implementation() internal view returns (address) {
        return IBeacon(_beacon()).implementation();
    }

    function _delegate(address impl) internal {
        assembly {
            calldatacopy(0, 0, calldatasize())
            let result := delegatecall(gas(), impl, 0, calldatasize(), 0, 0)
            returndatacopy(0, 0, returndatasize())
            switch result
            case 0 { revert(0, returndatasize()) }
            default { return(0, returndatasize()) }
        }
    }

    // ─── Fallback / receive ───────────────────────────────────────────────────

    fallback() external payable {
        _delegate(_implementation());
    }

    receive() external payable {
        _delegate(_implementation());
    }
}
