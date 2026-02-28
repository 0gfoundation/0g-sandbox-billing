// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title UpgradeableBeacon
/// @notice Stores the current implementation address for all BeaconProxy instances.
///         Owner can call upgradeTo() to point all proxies at a new implementation.
contract UpgradeableBeacon {

    address private _implementation;
    address private _owner;

    event Upgraded(address indexed implementation);
    event OwnershipTransferred(address indexed previousOwner, address indexed newOwner);

    constructor(address implementation_, address owner_) {
        require(implementation_.code.length > 0, "beacon: impl has no code");
        require(owner_ != address(0), "beacon: zero owner");
        _implementation = implementation_;
        _owner = owner_;
        emit OwnershipTransferred(address(0), owner_);
        emit Upgraded(implementation_);
    }

    function implementation() external view returns (address) {
        return _implementation;
    }

    function owner() external view returns (address) {
        return _owner;
    }

    function upgradeTo(address newImplementation) external {
        require(msg.sender == _owner, "beacon: not owner");
        require(newImplementation.code.length > 0, "beacon: impl has no code");
        _implementation = newImplementation;
        emit Upgraded(newImplementation);
    }

    function transferOwnership(address newOwner) external {
        require(msg.sender == _owner, "beacon: not owner");
        require(newOwner != address(0), "beacon: zero address");
        address prev = _owner;
        _owner = newOwner;
        emit OwnershipTransferred(prev, newOwner);
    }
}
