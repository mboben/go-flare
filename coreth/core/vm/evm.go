// (c) 2019-2020, Ava Labs, Inc.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package vm

import (
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/coreth/constants"
	"github.com/ava-labs/coreth/params"
	"github.com/ava-labs/coreth/precompile/contract"
	"github.com/ava-labs/coreth/precompile/modules"
	"github.com/ava-labs/coreth/precompile/precompileconfig"
	"github.com/ava-labs/coreth/predicate"
	"github.com/ava-labs/coreth/vmerrs"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

var (
	_ contract.AccessibleState = &EVM{}
	_ contract.BlockContext    = &BlockContext{}
)

// IsProhibited returns true if [addr] is in the prohibited list of addresses which should
// not be allowed as an EOA or newly created contract address.
func IsProhibited(addr common.Address) bool {
	if addr == constants.BlackholeAddr {
		return true
	}

	return modules.ReservedAddress(addr)
}

// emptyCodeHash is used by create to ensure deployment is disallowed to already
// deployed contract addresses (relevant after the account abstraction).
var emptyCodeHash = crypto.Keccak256Hash(nil)

type (
	// CanTransferFunc is the signature of a transfer guard function
	CanTransferFunc   func(StateDB, common.Address, *big.Int) bool
	CanTransferMCFunc func(StateDB, common.Address, common.Address, common.Hash, *big.Int) bool
	// TransferFunc is the signature of a transfer function
	TransferFunc   func(StateDB, common.Address, common.Address, *big.Int)
	TransferMCFunc func(StateDB, common.Address, common.Address, common.Hash, *big.Int)
	// GetHashFunc returns the n'th block hash in the blockchain
	// and is used by the BLOCKHASH EVM op code.
	GetHashFunc func(uint64) common.Hash
)

func (evm *EVM) precompile(addr common.Address) (contract.StatefulPrecompiledContract, bool) {
	var precompiles map[common.Address]contract.StatefulPrecompiledContract
	switch {
	case evm.chainRules.IsBanff:
		precompiles = PrecompiledContractsBanff
	case evm.chainRules.IsApricotPhase6:
		precompiles = PrecompiledContractsApricotPhase6
	case evm.chainRules.IsApricotPhasePre6:
		precompiles = PrecompiledContractsApricotPhasePre6
	case evm.chainRules.IsApricotPhase2:
		precompiles = PrecompiledContractsApricotPhase2
	case evm.chainRules.IsIstanbul:
		precompiles = PrecompiledContractsIstanbul
	case evm.chainRules.IsByzantium:
		precompiles = PrecompiledContractsByzantium
	default:
		precompiles = PrecompiledContractsHomestead
	}

	// Check the existing precompiles first
	p, ok := precompiles[addr]
	if ok {
		return p, true
	}

	// Otherwise, check the chain rules for the additionally configured precompiles.
	if _, ok = evm.chainRules.ActivePrecompiles[addr]; ok {
		module, ok := modules.GetPrecompileModuleByAddress(addr)
		return module.Contract, ok
	}

	return nil, false
}

// BlockContext provides the EVM with auxiliary information. Once provided
// it shouldn't be modified.
type BlockContext struct {
	// CanTransfer returns whether the account contains
	// sufficient ether to transfer the value
	CanTransfer CanTransferFunc
	// CanTransferMC returns whether the account contains
	// sufficient multicoin balance to transfer the value
	CanTransferMC CanTransferMCFunc
	// Transfer transfers ether from one account to the other
	Transfer TransferFunc
	// TransferMultiCoin transfers multicoin from one account to the other
	TransferMultiCoin TransferMCFunc
	// GetHash returns the hash corresponding to n
	GetHash GetHashFunc
	// PredicateResults are the results of predicate verification available throughout the EVM's execution.
	// PredicateResults may be nil if it is not encoded in the block's header.
	PredicateResults *predicate.Results

	// Block information
	Coinbase    common.Address // Provides information for COINBASE
	GasLimit    uint64         // Provides information for GASLIMIT
	BlockNumber *big.Int       // Provides information for NUMBER
	Time        uint64         // Provides information for TIME
	Difficulty  *big.Int       // Provides information for DIFFICULTY
	BaseFee     *big.Int       // Provides information for BASEFEE
}

func (b *BlockContext) Number() *big.Int {
	return b.BlockNumber
}

func (b *BlockContext) Timestamp() uint64 {
	return b.Time
}

func (b *BlockContext) GetPredicateResults(txHash common.Hash, address common.Address) []byte {
	if b.PredicateResults == nil {
		return nil
	}
	return b.PredicateResults.GetResults(txHash, address)
}

// TxContext provides the EVM with information about a transaction.
// All fields can change between transactions.
type TxContext struct {
	// Message information
	Origin   common.Address // Provides information for ORIGIN
	GasPrice *big.Int       // Provides information for GASPRICE
}

// EVM is the Ethereum Virtual Machine base object and provides
// the necessary tools to run a contract on the given state with
// the provided context. It should be noted that any error
// generated through any of the calls should be considered a
// revert-state-and-consume-all-gas operation, no checks on
// specific errors should ever be performed. The interpreter makes
// sure that any errors generated are to be considered faulty code.
//
// The EVM should never be reused and is not thread safe.
type EVM struct {
	// Context provides auxiliary blockchain related information
	Context BlockContext
	TxContext
	// StateDB gives access to the underlying state
	StateDB StateDB
	// Depth is the current call stack
	depth int

	// chainConfig contains information about the current chain
	chainConfig *params.ChainConfig
	// chain rules contains the chain rules for the current epoch
	chainRules params.Rules
	// virtual machine configuration options used to initialise the
	// evm.
	Config Config
	// global (to this context) ethereum virtual machine
	// used throughout the execution of the tx.
	interpreter *EVMInterpreter
	// abort is used to abort the EVM calling operations
	abort atomic.Bool
	// callGasTemp holds the gas available for the current call. This is needed because the
	// available gas is calculated in gasCall* according to the 63/64 rule and later
	// applied in opCall*.
	callGasTemp uint64
}

// NewEVM returns a new EVM. The returned EVM is not thread safe and should
// only ever be used *once*.
func NewEVM(blockCtx BlockContext, txCtx TxContext, statedb StateDB, chainConfig *params.ChainConfig, config Config) *EVM {
	evm := &EVM{
		Context:     blockCtx,
		TxContext:   txCtx,
		StateDB:     statedb,
		Config:      config,
		chainConfig: chainConfig,
		chainRules:  chainConfig.AvalancheRules(blockCtx.BlockNumber, blockCtx.Time),
	}
	evm.interpreter = NewEVMInterpreter(evm)
	return evm
}

// Reset resets the EVM with a new transaction context.Reset
// This is not threadsafe and should only be done very cautiously.
func (evm *EVM) Reset(txCtx TxContext, statedb StateDB) {
	evm.TxContext = txCtx
	evm.StateDB = statedb
}

// Cancel cancels any running EVM operation. This may be called concurrently and
// it's safe to be called multiple times.
func (evm *EVM) Cancel() {
	evm.abort.Store(true)
}

// Cancelled returns true if Cancel has been called
func (evm *EVM) Cancelled() bool {
	return evm.abort.Load()
}

// GetSnowContext returns the evm's snow.Context.
func (evm *EVM) GetSnowContext() *snow.Context {
	return evm.chainConfig.SnowCtx
}

// GetStateDB returns the evm's StateDB
func (evm *EVM) GetStateDB() contract.StateDB {
	return evm.StateDB
}

// GetBlockContext returns the evm's BlockContext
func (evm *EVM) GetBlockContext() contract.BlockContext {
	return &evm.Context
}

// Interpreter returns the current interpreter
func (evm *EVM) Interpreter() *EVMInterpreter {
	return evm.interpreter
}

// SetBlockContext updates the block context of the EVM.
func (evm *EVM) SetBlockContext(blockCtx BlockContext) {
	evm.Context = blockCtx
	num := blockCtx.BlockNumber
	evm.chainRules = evm.chainConfig.AvalancheRules(num, blockCtx.Time)
}

// DaemonCall separates a regular call from taking a snapshot and reverting to it in case of error.
// The function returns the snapshot in order to permit another opportunity for reverting to the
// snapshot in the event that the subsequent call to mint() in coreth/core/daemon.go fails.
func (evm *EVM) DaemonCall(caller ContractRef, addr common.Address, input []byte, gas uint64) (snapshot int, ret []byte, leftOverGas uint64, err error) {
	// Temporarily disable EVM debugging
	oldTracer := evm.Config.Tracer
	defer func() {
		evm.Config.Tracer = oldTracer
	}()
	evm.Config.Tracer = nil

	value := big.NewInt(0)
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return 0, nil, gas, vmerrs.ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	// Note: it is not possible for a negative value to be passed in here due to the fact
	// that [value] will be popped from the stack and decoded to a *big.Int, which will
	// always yield a positive result.
	if value.Sign() != 0 && !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		return 0, nil, gas, vmerrs.ErrInsufficientBalance
	}
	snapshot = evm.StateDB.Snapshot()

	// Perform Daemon Call
	ret, gas, err = evm.CallWithoutSnapshot(caller, addr, input, gas, value)

	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != vmerrs.ErrExecutionReverted {
			gas = 0
		}
	}
	return snapshot, ret, gas, err
}

// CallWithoutSnapshot executes the contract associated with the addr with the given input as
// parameters. It also handles any necessary value transfer required and takes
// the necessary steps to create accounts
func (evm *EVM) CallWithoutSnapshot(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	p, isPrecompile := evm.precompile(addr)

	if !evm.StateDB.Exist(addr) {
		if !isPrecompile && evm.chainRules.IsEIP158 && value.Sign() == 0 {
			// Calling a non existing account, don't do anything, but ping the tracer
			if evm.Config.Tracer != nil {
				if evm.depth == 0 {
					evm.Config.Tracer.CaptureStart(evm, caller.Address(), addr, false, input, gas, value)
					evm.Config.Tracer.CaptureEnd(ret, 0, nil)
				} else {
					evm.Config.Tracer.CaptureEnter(CALL, caller.Address(), addr, input, gas, value)
					evm.Config.Tracer.CaptureExit(ret, 0, nil)
				}
			}
			return nil, gas, nil
		}
		evm.StateDB.CreateAccount(addr)
	}
	evm.Context.Transfer(evm.StateDB, caller.Address(), addr, value)

	// Capture the tracer start/end events in debug mode
	if evm.Config.Tracer != nil {
		if evm.depth == 0 {
			evm.Config.Tracer.CaptureStart(evm, caller.Address(), addr, false, input, gas, value)
			defer func(startGas uint64, startTime time.Time) { // Lazy evaluation of the parameters
				evm.Config.Tracer.CaptureEnd(ret, startGas-gas, err)
			}(gas, time.Now())
		} else {
			// Handle tracer events for entering and exiting a call frame
			evm.Config.Tracer.CaptureEnter(CALL, caller.Address(), addr, input, gas, value)
			defer func(startGas uint64) {
				evm.Config.Tracer.CaptureExit(ret, startGas-gas, err)
			}(gas)
		}
	}

	if isPrecompile {
		ret, gas, err = RunStatefulPrecompiledContract(p, evm, caller.Address(), addr, input, gas, evm.interpreter.readOnly)
	} else {
		// Initialise a new contract and set the code that is to be used by the EVM.
		// The contract is a scoped environment for this execution context only.
		code := evm.StateDB.GetCode(addr)
		if len(code) == 0 {
			ret, err = nil, nil // gas is unchanged
		} else {
			addrCopy := addr
			// If the account has no code, we can abort here
			// The depth-check is already done, and precompiles handled above
			contract := NewContract(caller, AccountRef(addrCopy), value, gas)
			contract.SetCallCode(&addrCopy, evm.StateDB.GetCodeHash(addrCopy), code)
			ret, err = evm.interpreter.Run(contract, input, false)
			gas = contract.Gas
		}
	}
	return ret, gas, err
}

// Call executes the contract associated with the addr with the given input as
// parameters. It also handles any necessary value transfer required and takes
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
func (evm *EVM) Call(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, vmerrs.ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	// Note: it is not possible for a negative value to be passed in here due to the fact
	// that [value] will be popped from the stack and decoded to a *big.Int, which will
	// always yield a positive result.
	if value.Sign() != 0 && !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, gas, vmerrs.ErrInsufficientBalance
	}
	snapshot := evm.StateDB.Snapshot()
	p, isPrecompile := evm.precompile(addr)
	debug := evm.Config.Tracer != nil

	if !evm.StateDB.Exist(addr) {
		if !isPrecompile && evm.chainRules.IsEIP158 && value.Sign() == 0 {
			// Calling a non existing account, don't do anything, but ping the tracer
			if debug {
				if evm.depth == 0 {
					evm.Config.Tracer.CaptureStart(evm, caller.Address(), addr, false, input, gas, value)
					evm.Config.Tracer.CaptureEnd(ret, 0, nil)
				} else {
					evm.Config.Tracer.CaptureEnter(CALL, caller.Address(), addr, input, gas, value)
					evm.Config.Tracer.CaptureExit(ret, 0, nil)
				}
			}
			return nil, gas, nil
		}
		evm.StateDB.CreateAccount(addr)
	}
	evm.Context.Transfer(evm.StateDB, caller.Address(), addr, value)

	// Capture the tracer start/end events in debug mode
	if debug {
		if evm.depth == 0 {
			evm.Config.Tracer.CaptureStart(evm, caller.Address(), addr, false, input, gas, value)
			defer func(startGas uint64) { // Lazy evaluation of the parameters
				evm.Config.Tracer.CaptureEnd(ret, startGas-gas, err)
			}(gas)
		} else {
			// Handle tracer events for entering and exiting a call frame
			evm.Config.Tracer.CaptureEnter(CALL, caller.Address(), addr, input, gas, value)
			defer func(startGas uint64) {
				evm.Config.Tracer.CaptureExit(ret, startGas-gas, err)
			}(gas)
		}
	}

	if isPrecompile {
		ret, gas, err = RunStatefulPrecompiledContract(p, evm, caller.Address(), addr, input, gas, evm.interpreter.readOnly)
	} else {
		// Initialise a new contract and set the code that is to be used by the EVM.
		// The contract is a scoped environment for this execution context only.
		code := evm.StateDB.GetCode(addr)
		if len(code) == 0 {
			ret, err = nil, nil // gas is unchanged
		} else {
			addrCopy := addr
			// If the account has no code, we can abort here
			// The depth-check is already done, and precompiles handled above
			contract := NewContract(caller, AccountRef(addrCopy), value, gas)
			contract.SetCallCode(&addrCopy, evm.StateDB.GetCodeHash(addrCopy), code)
			ret, err = evm.interpreter.Run(contract, input, false)
			gas = contract.Gas
		}
	}
	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != vmerrs.ErrExecutionReverted {
			gas = 0
		}
		// TODO: consider clearing up unused snapshots:
		//} else {
		//	evm.StateDB.DiscardSnapshot(snapshot)
	}
	return ret, gas, err
}

// This allows the user transfer balance of a specified coinId in addition to a normal Call().
func (evm *EVM) CallExpert(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int, coinID common.Hash, value2 *big.Int) (ret []byte, leftOverGas uint64, err error) {
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, vmerrs.ErrDepth
	}

	// Fail if we're trying to transfer more than the available balance
	// Note: it is not possible for a negative value to be passed in here due to the fact
	// that [value] will be popped from the stack and decoded to a *big.Int, which will
	// always yield a positive result.
	if value.Sign() != 0 && !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, gas, vmerrs.ErrInsufficientBalance
	}

	if value2.Sign() != 0 && !evm.Context.CanTransferMC(evm.StateDB, caller.Address(), addr, coinID, value2) {
		return nil, gas, vmerrs.ErrInsufficientBalance
	}

	snapshot := evm.StateDB.Snapshot()
	//p, isPrecompile := evm.precompile(addr)

	if !evm.StateDB.Exist(addr) {
		//if !isPrecompile && evm.chainRules.IsEIP158 && value.Sign() == 0 {
		//	// Calling a non existing account, don't do anything, but ping the tracer
		//	if evm.Config.Debug && evm.depth == 0 {
		//		evm.Config.Tracer.CaptureStart(evm, caller.Address(), addr, false, input, gas, value)
		//		evm.Config.Tracer.CaptureEnd(ret, 0, 0, nil)
		//	}
		//	return nil, gas, nil
		//}
		evm.StateDB.CreateAccount(addr)
	}
	evm.Context.Transfer(evm.StateDB, caller.Address(), addr, value)
	evm.Context.TransferMultiCoin(evm.StateDB, caller.Address(), addr, coinID, value2)

	// Capture the tracer start/end events in debug mode
	debug := evm.Config.Tracer != nil
	if debug && evm.depth == 0 {
		evm.Config.Tracer.CaptureStart(evm, caller.Address(), addr, false, input, gas, value)
		defer func(startGas uint64, startTime time.Time) { // Lazy evaluation of the parameters
			evm.Config.Tracer.CaptureEnd(ret, startGas-gas, err)
		}(gas, time.Now())
	}

	//if isPrecompile {
	//	ret, gas, err = RunPrecompiledContract(p, input, gas)
	//} else {
	// Initialise a new contract and set the code that is to be used by the EVM.
	// The contract is a scoped environment for this execution context only.
	code := evm.StateDB.GetCode(addr)
	if len(code) == 0 {
		ret, err = nil, nil // gas is unchanged
	} else {
		addrCopy := addr
		// If the account has no code, we can abort here
		// The depth-check is already done, and precompiles handled above
		contract := NewContract(caller, AccountRef(addrCopy), value, gas)
		contract.SetCallCode(&addrCopy, evm.StateDB.GetCodeHash(addrCopy), code)
		ret, err = evm.interpreter.Run(contract, input, false)
		gas = contract.Gas
	}
	//}
	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != vmerrs.ErrExecutionReverted {
			gas = 0
		}
		// TODO: consider clearing up unused snapshots:
		//} else {
		//	evm.StateDB.DiscardSnapshot(snapshot)
	}
	return ret, gas, err
}

// CallCode executes the contract associated with the addr with the given input
// as parameters. It also handles any necessary value transfer required and takes
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
//
// CallCode differs from Call in the sense that it executes the given address'
// code with the caller as context.
func (evm *EVM) CallCode(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, vmerrs.ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	// Note although it's noop to transfer X ether to caller itself. But
	// if caller doesn't have enough balance, it would be an error to allow
	// over-charging itself. So the check here is necessary.
	// Note: it is not possible for a negative value to be passed in here due to the fact
	// that [value] will be popped from the stack and decoded to a *big.Int, which will
	// always yield a positive result.
	if !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, gas, vmerrs.ErrInsufficientBalance
	}
	var snapshot = evm.StateDB.Snapshot()

	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		evm.Config.Tracer.CaptureEnter(CALLCODE, caller.Address(), addr, input, gas, value)
		defer func(startGas uint64) {
			evm.Config.Tracer.CaptureExit(ret, startGas-gas, err)
		}(gas)
	}

	// It is allowed to call precompiles, even via delegatecall
	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunStatefulPrecompiledContract(p, evm, caller.Address(), addr, input, gas, evm.interpreter.readOnly)
	} else {
		addrCopy := addr
		// Initialise a new contract and set the code that is to be used by the EVM.
		// The contract is a scoped environment for this execution context only.
		contract := NewContract(caller, AccountRef(caller.Address()), value, gas)
		contract.SetCallCode(&addrCopy, evm.StateDB.GetCodeHash(addrCopy), evm.StateDB.GetCode(addrCopy))
		ret, err = evm.interpreter.Run(contract, input, false)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != vmerrs.ErrExecutionReverted {
			gas = 0
		}
	}
	return ret, gas, err
}

// DelegateCall executes the contract associated with the addr with the given input
// as parameters. It reverses the state in case of an execution error.
//
// DelegateCall differs from CallCode in the sense that it executes the given address'
// code with the caller as context and the caller is set to the caller of the caller.
func (evm *EVM) DelegateCall(caller ContractRef, addr common.Address, input []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, vmerrs.ErrDepth
	}
	var snapshot = evm.StateDB.Snapshot()

	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		// NOTE: caller must, at all times be a contract. It should never happen
		// that caller is something other than a Contract.
		parent := caller.(*Contract)
		// DELEGATECALL inherits value from parent call
		evm.Config.Tracer.CaptureEnter(DELEGATECALL, caller.Address(), addr, input, gas, parent.value)
		defer func(startGas uint64) {
			evm.Config.Tracer.CaptureExit(ret, startGas-gas, err)
		}(gas)
	}

	// It is allowed to call precompiles, even via delegatecall
	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunStatefulPrecompiledContract(p, evm, caller.Address(), addr, input, gas, evm.interpreter.readOnly)
	} else {
		addrCopy := addr
		// Initialise a new contract and make initialise the delegate values
		contract := NewContract(caller, AccountRef(caller.Address()), nil, gas).AsDelegate()
		contract.SetCallCode(&addrCopy, evm.StateDB.GetCodeHash(addrCopy), evm.StateDB.GetCode(addrCopy))
		ret, err = evm.interpreter.Run(contract, input, false)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != vmerrs.ErrExecutionReverted {
			gas = 0
		}
	}
	return ret, gas, err
}

// StaticCall executes the contract associated with the addr with the given input
// as parameters while disallowing any modifications to the state during the call.
// Opcodes that attempt to perform such modifications will result in exceptions
// instead of performing the modifications.
func (evm *EVM) StaticCall(caller ContractRef, addr common.Address, input []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, vmerrs.ErrDepth
	}
	// We take a snapshot here. This is a bit counter-intuitive, and could probably be skipped.
	// However, even a staticcall is considered a 'touch'. On mainnet, static calls were introduced
	// after all empty accounts were deleted, so this is not required. However, if we omit this,
	// then certain tests start failing; stRevertTest/RevertPrecompiledTouchExactOOG.json.
	// We could change this, but for now it's left for legacy reasons
	var snapshot = evm.StateDB.Snapshot()

	// We do an AddBalance of zero here, just in order to trigger a touch.
	// This doesn't matter on Mainnet, where all empties are gone at the time of Byzantium,
	// but is the correct thing to do and matters on other networks, in tests, and potential
	// future scenarios
	evm.StateDB.AddBalance(addr, big0)

	// Invoke tracer hooks that signal entering/exiting a call frame
	if evm.Config.Tracer != nil {
		evm.Config.Tracer.CaptureEnter(STATICCALL, caller.Address(), addr, input, gas, nil)
		defer func(startGas uint64) {
			evm.Config.Tracer.CaptureExit(ret, startGas-gas, err)
		}(gas)
	}

	if p, isPrecompile := evm.precompile(addr); isPrecompile {
		ret, gas, err = RunStatefulPrecompiledContract(p, evm, caller.Address(), addr, input, gas, true)
	} else {
		// At this point, we use a copy of address. If we don't, the go compiler will
		// leak the 'contract' to the outer scope, and make allocation for 'contract'
		// even if the actual execution ends on RunPrecompiled above.
		addrCopy := addr
		// Initialise a new contract and set the code that is to be used by the EVM.
		// The contract is a scoped environment for this execution context only.
		contract := NewContract(caller, AccountRef(addrCopy), new(big.Int), gas)
		contract.SetCallCode(&addrCopy, evm.StateDB.GetCodeHash(addrCopy), evm.StateDB.GetCode(addrCopy))
		// When an error was returned by the EVM or when setting the creation code
		// above we revert to the snapshot and consume any gas remaining. Additionally
		// when we're in Homestead this also counts for code storage gas errors.
		ret, err = evm.interpreter.Run(contract, input, true)
		gas = contract.Gas
	}
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != vmerrs.ErrExecutionReverted {
			gas = 0
		}
	}
	return ret, gas, err
}

type codeAndHash struct {
	code []byte
	hash common.Hash
}

func (c *codeAndHash) Hash() common.Hash {
	if c.hash == (common.Hash{}) {
		c.hash = crypto.Keccak256Hash(c.code)
	}
	return c.hash
}

// create creates a new contract using code as deployment code.
func (evm *EVM) create(caller ContractRef, codeAndHash *codeAndHash, gas uint64, value *big.Int, address common.Address, typ OpCode) ([]byte, common.Address, uint64, error) {
	// Depth check execution. Fail if we're trying to execute above the
	// limit.
	if evm.depth > int(params.CallCreateDepth) {
		return nil, common.Address{}, gas, vmerrs.ErrDepth
	}
	// Note: it is not possible for a negative value to be passed in here due to the fact
	// that [value] will be popped from the stack and decoded to a *big.Int, which will
	// always yield a positive result.
	if !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, common.Address{}, gas, vmerrs.ErrInsufficientBalance
	}
	// If there is any collision with a prohibited address, return an error instead
	// of allowing the contract to be created.
	if IsProhibited(address) {
		return nil, common.Address{}, gas, vmerrs.ErrAddrProhibited
	}
	nonce := evm.StateDB.GetNonce(caller.Address())
	if nonce+1 < nonce {
		return nil, common.Address{}, gas, vmerrs.ErrNonceUintOverflow
	}
	evm.StateDB.SetNonce(caller.Address(), nonce+1)
	// We add this to the access list _before_ taking a snapshot. Even if the creation fails,
	// the access-list change should not be rolled back
	if evm.chainRules.IsApricotPhase2 {
		evm.StateDB.AddAddressToAccessList(address)
	}
	// Ensure there's no existing contract already at the designated address
	contractHash := evm.StateDB.GetCodeHash(address)
	if evm.StateDB.GetNonce(address) != 0 || (contractHash != (common.Hash{}) && contractHash != emptyCodeHash) {
		return nil, common.Address{}, 0, vmerrs.ErrContractAddressCollision
	}
	// Create a new account on the state
	snapshot := evm.StateDB.Snapshot()
	evm.StateDB.CreateAccount(address)
	if evm.chainRules.IsEIP158 {
		evm.StateDB.SetNonce(address, 1)
	}
	evm.Context.Transfer(evm.StateDB, caller.Address(), address, value)

	// Initialise a new contract and set the code that is to be used by the EVM.
	// The contract is a scoped environment for this execution context only.
	contract := NewContract(caller, AccountRef(address), value, gas)
	contract.SetCodeOptionalHash(&address, codeAndHash)

	if evm.Config.Tracer != nil {
		if evm.depth == 0 {
			evm.Config.Tracer.CaptureStart(evm, caller.Address(), address, true, codeAndHash.code, gas, value)
		} else {
			evm.Config.Tracer.CaptureEnter(typ, caller.Address(), address, codeAndHash.code, gas, value)
		}
	}

	ret, err := evm.interpreter.Run(contract, nil, false)

	// Check whether the max code size has been exceeded, assign err if the case.
	if err == nil && evm.chainRules.IsEIP158 && len(ret) > params.MaxCodeSize {
		err = vmerrs.ErrMaxCodeSizeExceeded
	}

	// Reject code starting with 0xEF if EIP-3541 is enabled.
	if err == nil && len(ret) >= 1 && ret[0] == 0xEF && evm.chainRules.IsApricotPhase3 {
		err = vmerrs.ErrInvalidCode
	}

	// if the contract creation ran successfully and no errors were returned
	// calculate the gas required to store the code. If the code could not
	// be stored due to not enough gas set an error and let it be handled
	// by the error checking condition below.
	if err == nil {
		createDataGas := uint64(len(ret)) * params.CreateDataGas
		if contract.UseGas(createDataGas) {
			evm.StateDB.SetCode(address, ret)
		} else {
			err = vmerrs.ErrCodeStoreOutOfGas
		}
	}

	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if err != nil && (evm.chainRules.IsHomestead || err != vmerrs.ErrCodeStoreOutOfGas) {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != vmerrs.ErrExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}

	if evm.Config.Tracer != nil {
		if evm.depth == 0 {
			evm.Config.Tracer.CaptureEnd(ret, gas-contract.Gas, err)
		} else {
			evm.Config.Tracer.CaptureExit(ret, gas-contract.Gas, err)
		}
	}
	return ret, address, contract.Gas, err
}

// Create creates a new contract using code as deployment code.
func (evm *EVM) Create(caller ContractRef, code []byte, gas uint64, value *big.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	contractAddr = crypto.CreateAddress(caller.Address(), evm.StateDB.GetNonce(caller.Address()))
	return evm.create(caller, &codeAndHash{code: code}, gas, value, contractAddr, CREATE)
}

// Create2 creates a new contract using code as deployment code.
//
// The different between Create2 with Create is Create2 uses keccak256(0xff ++ msg.sender ++ salt ++ keccak256(init_code))[12:]
// instead of the usual sender-and-nonce-hash as the address where the contract is initialized at.
func (evm *EVM) Create2(caller ContractRef, code []byte, gas uint64, endowment *big.Int, salt *uint256.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	codeAndHash := &codeAndHash{code: code}
	contractAddr = crypto.CreateAddress2(caller.Address(), salt.Bytes32(), codeAndHash.Hash().Bytes())
	return evm.create(caller, codeAndHash, gas, endowment, contractAddr, CREATE2)
}

// ChainConfig returns the environment's chain configuration
func (evm *EVM) ChainConfig() *params.ChainConfig { return evm.chainConfig }

// GetChainConfig implements AccessibleState
func (evm *EVM) GetChainConfig() precompileconfig.ChainConfig { return evm.chainConfig }

func (evm *EVM) NativeAssetCall(caller common.Address, input []byte, suppliedGas uint64, gasCost uint64, readOnly bool) (ret []byte, remainingGas uint64, err error) {
	if suppliedGas < gasCost {
		return nil, 0, vmerrs.ErrOutOfGas
	}
	remainingGas = suppliedGas - gasCost

	if readOnly {
		return nil, remainingGas, vmerrs.ErrExecutionReverted
	}

	to, assetID, assetAmount, callData, err := UnpackNativeAssetCallInput(input)
	if err != nil {
		return nil, remainingGas, vmerrs.ErrExecutionReverted
	}

	// Note: it is not possible for a negative assetAmount to be passed in here due to the fact that decoding a
	// byte slice into a *big.Int type will always return a positive value.
	if assetAmount.Sign() != 0 && !evm.Context.CanTransferMC(evm.StateDB, caller, to, assetID, assetAmount) {
		return nil, remainingGas, vmerrs.ErrInsufficientBalance
	}

	snapshot := evm.StateDB.Snapshot()

	if !evm.StateDB.Exist(to) {
		if remainingGas < params.CallNewAccountGas {
			return nil, 0, vmerrs.ErrOutOfGas
		}
		remainingGas -= params.CallNewAccountGas
		evm.StateDB.CreateAccount(to)
	}

	// Increment the call depth which is restricted to 1024
	evm.depth++
	defer func() { evm.depth-- }()

	// Send [assetAmount] of [assetID] to [to] address
	evm.Context.TransferMultiCoin(evm.StateDB, caller, to, assetID, assetAmount)
	ret, remainingGas, err = evm.Call(AccountRef(caller), to, callData, remainingGas, new(big.Int))

	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != vmerrs.ErrExecutionReverted {
			remainingGas = 0
		}
		// TODO: consider clearing up unused snapshots:
		//} else {
		//	evm.StateDB.DiscardSnapshot(snapshot)
	}
	return ret, remainingGas, err
}
