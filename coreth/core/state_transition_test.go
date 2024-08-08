package core

import (
	"math/big"
	"testing"

	"github.com/ava-labs/coreth/core/rawdb"
	"github.com/ava-labs/coreth/core/state"
	"github.com/ava-labs/coreth/core/state/snapshot"
	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/core/vm"
	"github.com/ava-labs/coreth/eth/tracers/logger"
	"github.com/ava-labs/coreth/ethdb"
	"github.com/ava-labs/coreth/params"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Test prioritized contract (Submission) being partially refunded when fee is high
func TestStateTransition(t *testing.T) {
	configs := []*params.ChainConfig{params.CostonChainConfig, params.CostwoChainConfig, params.SongbirdChainConfig, params.FlareChainConfig}

	for _, config := range configs {
		key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		from := crypto.PubkeyToAddress(key.PublicKey)
		gas := uint64(3000000) // 1M gas
		to := common.HexToAddress("0x2cA6571Daa15ce734Bbd0Bf27D5C9D16787fc33f")
		daemon := common.HexToAddress(GetDaemonContractAddr(new(big.Int).SetUint64(1729208000)))
		signer := types.LatestSignerForChainID(big.NewInt(config.ChainID.Int64()))
		tx, err := types.SignNewTx(key, signer,
			&types.LegacyTx{
				Nonce:    1,
				GasPrice: big.NewInt(1250000000000000),
				Gas:      gas,
				// Data:     common.FromHex("f613a687"),
				To: &to,
			})
		if err != nil {
			t.Fatal(err)
		}
		txContext := vm.TxContext{
			Origin:   from,
			GasPrice: tx.GasPrice(),
		}
		context := vm.BlockContext{
			CanTransfer: CanTransfer,
			Transfer:    Transfer,
			// Coinbase address is mostly for SGB and Coston
			Coinbase:    common.HexToAddress("0x0100000000000000000000000000000000000000"),
			BlockNumber: new(big.Int).SetUint64(uint64(5)),
			Time:        new(big.Int).SetUint64(1729208000),
			Difficulty:  big.NewInt(0xffffffff),
			GasLimit:    gas,
			BaseFee:     big.NewInt(8),
		}
		alloc := GenesisAlloc{}
		balance := new(big.Int)
		balance.SetString("10000000000000000000000000000000000", 10)
		alloc[from] = GenesisAccount{
			Nonce:   1,
			Code:    []byte{},
			Balance: balance,
		}
		alloc[to] = GenesisAccount{
			Nonce:   2,
			Code:    code,
			Balance: balance,
		}
		alloc[daemon] = GenesisAccount{
			Nonce:   3,
			Code:    daemonCode,
			Balance: balance,
		}
		_, statedb := MakePreState(rawdb.NewMemoryDatabase(), alloc, false)
		// Create the tracer, the EVM environment and run it
		tracer := logger.NewStructLogger(&logger.Config{
			Debug: false,
		})
		cfg := vm.Config{Debug: true, Tracer: tracer}
		evm := vm.NewEVM(context, txContext, statedb, config, cfg)
		msg, err := tx.AsMessage(signer, nil)
		if err != nil {
			t.Fatalf("failed to prepare transaction for tracing: %v", err)
		}

		st := NewStateTransition(evm, msg, new(GasPool).AddGas(tx.Gas()))

		firstBalance := st.state.GetBalance(st.msg.From())

		_, err = st.TransitionDb()
		if err != nil {
			t.Fatal(err)
		}
		secondBalance := st.state.GetBalance(st.msg.From())

		chainID := st.evm.ChainConfig().ChainID

		// max fee (funds above which are returned) depends on the chain used
		_, limit, _, _, _ := stateTransitionVariants.GetValue(chainID)(st)
		maxFee := new(big.Int).Mul(new(big.Int).SetUint64(params.TxGas), new(big.Int).SetUint64(limit))
		diff := new(big.Int).Sub(firstBalance, secondBalance)

		if maxFee.Cmp(diff) != 0 {
			t.Fatalf(`want = %t, have %t.`, maxFee, diff)
		}
	}
}

// Test that daemon contract is invoked on a statetransition
func TestStateTransitionDaemon(t *testing.T) {
	configs := []*params.ChainConfig{params.CostonChainConfig, params.CostwoChainConfig, params.SongbirdChainConfig, params.FlareChainConfig}

	for _, config := range configs {
		key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		from := crypto.PubkeyToAddress(key.PublicKey)
		gas := uint64(3000000) // 1M gas
		daemon := common.HexToAddress(GetDaemonContractAddr(new(big.Int).SetUint64(1729208000)))
		to := common.HexToAddress("0x2cA6571Daa15ce734Bbd0Bf27D5C9D16787fc33f")
		signer := types.LatestSignerForChainID(big.NewInt(config.ChainID.Int64()))
		tx, err := types.SignNewTx(key, signer,
			&types.LegacyTx{
				Nonce:    1,
				GasPrice: big.NewInt(1250000000000000),
				Gas:      gas,
				// Data:     common.FromHex("f613a687"),
				To: &to,
			})
		if err != nil {
			t.Fatal(err)
		}
		txContext := vm.TxContext{
			Origin:   from,
			GasPrice: tx.GasPrice(),
		}
		context := vm.BlockContext{
			CanTransfer: CanTransfer,
			Transfer:    Transfer,
			// Coinbase address is mostly for SGB and Coston
			Coinbase:    common.HexToAddress("0x0100000000000000000000000000000000000000"),
			BlockNumber: new(big.Int).SetUint64(uint64(5)),
			Time:        new(big.Int).SetUint64(1729208000),
			Difficulty:  big.NewInt(0xffffffff),
			GasLimit:    gas,
			BaseFee:     big.NewInt(8),
		}
		alloc := GenesisAlloc{}
		balance := new(big.Int)
		balance.SetString("10000000000000000000000000000000000", 10)
		alloc[from] = GenesisAccount{
			Nonce:   1,
			Code:    []byte{},
			Balance: balance,
		}
		alloc[to] = GenesisAccount{
			Nonce:   2,
			Code:    code,
			Balance: balance,
		}
		alloc[daemon] = GenesisAccount{
			Nonce:   3,
			Code:    daemonCode,
			Balance: balance,
		}
		_, statedb := MakePreState(rawdb.NewMemoryDatabase(), alloc, false)
		// Create the tracer, the EVM environment and run it
		tracer := logger.NewStructLogger(&logger.Config{
			Debug: false,
		})
		cfg := vm.Config{Debug: true, Tracer: tracer}
		evm := vm.NewEVM(context, txContext, statedb, config, cfg)
		msg, err := tx.AsMessage(signer, nil)
		if err != nil {
			t.Fatalf("failed to prepare transaction for tracing: %v", err)
		}

		st := NewStateTransition(evm, msg, new(GasPool).AddGas(tx.Gas()))

		firstBalance := st.state.GetBalance(daemon)

		_, err = st.TransitionDb()
		if err != nil {
			t.Fatal(err)
		}

		secondBalance := st.state.GetBalance(daemon)
		diff := new(big.Int).Sub(firstBalance, secondBalance)

		if diff == new(big.Int).SetUint64(0) {
			t.Fatalf(`want nonzero, have %t.`, diff)
		}
	}
}

// this is a copy of the function from tests/???_utils.go, to create a starting state for test EVM
func MakePreState(db ethdb.Database, accounts GenesisAlloc, snapshotter bool) (*snapshot.Tree, *state.StateDB) {
	sdb := state.NewDatabase(db)
	statedb, _ := state.New(common.Hash{}, sdb, nil)
	for addr, a := range accounts {
		statedb.SetCode(addr, a.Code)
		statedb.SetNonce(addr, a.Nonce)
		statedb.SetBalance(addr, a.Balance)
		for k, v := range a.Storage {
			statedb.SetState(addr, k, v)
		}
	}
	// Commit and re-open to start with a clean state.
	root, _ := statedb.Commit(false, false)

	var snaps *snapshot.Tree
	if snapshotter {
		snaps, _ = snapshot.New(db, sdb.TrieDB(), 1, common.Hash{}, root, false, true, false)
	}
	statedb, _ = state.New(root, sdb, snaps)
	return snaps, statedb
}

// this is simple EVM code that returns 0x01 (necessary for contract to be refunded) and
// 0x01 is also the usual return value in prod (but check is only made for the value being != 0)
var code = []byte{
	byte(vm.PUSH1), 0x01, byte(vm.PUSH1), 0x0, byte(vm.MSTORE8), // store 1 memory at offset 0 for return
	byte(vm.PUSH1), 0x01, byte(vm.PUSH1), 0x0, // set return value size to 1, offset 0
	byte(vm.RETURN), // return 0x01
}

// return a 32-bit value, set to 1 for daemon balance change
var daemonCode = []byte{
	byte(vm.PUSH1), 0x01, byte(vm.PUSH1), 0x0, byte(vm.MSTORE), // store 1 memory at offset 0 for return
	byte(vm.PUSH1), 0x20, byte(vm.PUSH1), 0x0, // set return value size to 32 bits, offset 0
	byte(vm.RETURN), // return 0x0..01
}

// bytecode for a contract that has a function "0xf613a687" which always returns true
// but when using this as code for Contract in this test file, it returns itself (the contract bytecode),
// as if it were deploying itself (this is ok for the test,
// because the only thing that matters right now is that the return value is different from 0)
var bytecode string = "6080604052348015600f57600080fd5b50607780601d6000396000f3fe6080604052348015600f57600080fd5b506004361060285760003560e01c8063f613a68714602d575b600080fd5b604080516001815290519081900360200190f3fea2646970667358221220210fdc54d39137aa003fab3c5da424a68b30a53d26cd2db782eb91b2c005c1f164736f6c63430008140033"
