package precompiles

import (
	"errors"
	"math/big"

	sdkmath "cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"

	errorsmod "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"
	errortypes "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/evmos/ethermint/x/evm/types"
)

const (
	EVMDenomPrefix          = "evm/"
	BankContractRequiredGas = 10000
)

var (
	BankContractAddress = common.BytesToAddress([]byte{100})
	MintMethod          abi.Method
	BurnMethod          abi.Method
	BalanceOfMethod     abi.Method
	TransferMethod      abi.Method
)

func init() {
	addressType, _ := abi.NewType("address", "", nil)
	uint256Type, _ := abi.NewType("uint256", "", nil)
	MintMethod = abi.NewMethod(
		"mint", "mint", abi.Function, "", false, false, abi.Arguments{abi.Argument{
			Name: "recipient",
			Type: addressType,
		}, abi.Argument{
			Name: "amount",
			Type: uint256Type,
		}},
		nil,
	)
	BurnMethod = abi.NewMethod(
		"burn", "burn", abi.Function, "", false, false, abi.Arguments{abi.Argument{
			Name: "recipient",
			Type: addressType,
		}, abi.Argument{
			Name: "amount",
			Type: uint256Type,
		}},
		nil,
	)
	BalanceOfMethod = abi.NewMethod(
		"balanceOf", "balanceOf", abi.Function, "", false, false, abi.Arguments{abi.Argument{
			Name: "token",
			Type: addressType,
		}, abi.Argument{
			Name: "address",
			Type: addressType,
		}},
		abi.Arguments{abi.Argument{
			Name: "amount",
			Type: uint256Type,
		}},
	)
	TransferMethod = abi.NewMethod(
		"transfer", "transfer", abi.Function, "", false, false, abi.Arguments{abi.Argument{
			Name: "sender",
			Type: addressType,
		}, abi.Argument{
			Name: "recipient",
			Type: addressType,
		}, abi.Argument{
			Name: "amount",
			Type: uint256Type,
		}},
		nil,
	)
}

func EVMDenom(token common.Address) string {
	return EVMDenomPrefix + token.Hex()
}

type BankContract struct {
	bankKeeper types.BankKeeper
	cdc        codec.Codec
}

// NewBankContract creates the precompiled contract to manage native tokens
func NewBankContract(bankKeeper types.BankKeeper, cdc codec.Codec) vm.PrecompiledContract {
	return &BankContract{bankKeeper, cdc}
}

func (bc *BankContract) Address() common.Address {
	return BankContractAddress
}

// RequiredGas calculates the contract gas use
func (bc *BankContract) RequiredGas(input []byte) uint64 {
	// TODO estimate required gas
	return BankContractRequiredGas
}

func (bc *BankContract) IsStateful() bool {
	return true
}

func (bc *BankContract) checkBlockedAddr(addr sdk.AccAddress) error {
	to, err := sdk.AccAddressFromBech32(addr.String())
	if err != nil {
		return err
	}
	if bc.bankKeeper.BlockedAddr(to) {
		return errorsmod.Wrapf(errortypes.ErrUnauthorized, "%s is not allowed to receive funds", to.String())
	}
	return nil
}

func (bc *BankContract) Run(evm *vm.EVM, contract *vm.Contract, readonly bool) ([]byte, error) {
	// parse input
	methodID := contract.Input[:4]
	stateDB := evm.StateDB.(ExtStateDB)
	precompileAddr := bc.Address()

	switch string(methodID) {
	case string(MintMethod.ID), string(BurnMethod.ID):
		if readonly {
			return nil, errors.New("the method is not readonly")
		}
		mint := string(methodID) == string(MintMethod.ID)
		var method abi.Method
		if mint {
			method = MintMethod
		} else {
			method = BurnMethod
		}
		args, err := method.Inputs.Unpack(contract.Input[4:])
		if err != nil {
			return nil, errors.New("fail to unpack input arguments")
		}
		recipient := args[0].(common.Address)
		amount := args[1].(*big.Int)
		if amount.Sign() <= 0 {
			return nil, errors.New("invalid amount")
		}
		addr := sdk.AccAddress(recipient.Bytes())
		if err := bc.checkBlockedAddr(addr); err != nil {
			return nil, err
		}
		denom := EVMDenom(contract.CallerAddress)
		amt := sdk.NewCoin(denom, sdkmath.NewIntFromBigInt(amount))
		err = stateDB.ExecuteNativeAction(precompileAddr, nil, func(ctx sdk.Context) error {
			if err := bc.bankKeeper.IsSendEnabledCoins(ctx, amt); err != nil {
				return err
			}
			if mint {
				if err := bc.bankKeeper.MintCoins(ctx, types.ModuleName, sdk.NewCoins(amt)); err != nil {
					return errorsmod.Wrap(err, "fail to mint coins in precompiled contract")
				}
				if err := bc.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, addr, sdk.NewCoins(amt)); err != nil {
					return errorsmod.Wrap(err, "fail to send mint coins to account")
				}
			} else {
				if err := bc.bankKeeper.SendCoinsFromAccountToModule(ctx, addr, types.ModuleName, sdk.NewCoins(amt)); err != nil {
					return errorsmod.Wrap(err, "fail to send burn coins to module")
				}
				if err := bc.bankKeeper.BurnCoins(ctx, types.ModuleName, sdk.NewCoins(amt)); err != nil {
					return errorsmod.Wrap(err, "fail to burn coins in precompiled contract")
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	case string(BalanceOfMethod.ID):
		args, err := BalanceOfMethod.Inputs.Unpack(contract.Input[4:])
		if err != nil {
			return nil, errors.New("fail to unpack input arguments")
		}
		token := args[0].(common.Address)
		addr := args[1].(common.Address)
		// query from storage
		balance := big.NewInt(0)
		err = stateDB.ExecuteNativeAction(precompileAddr, nil, func(ctx sdk.Context) error {
			balance = bc.bankKeeper.GetBalance(ctx, sdk.AccAddress(addr.Bytes()), EVMDenom(token)).Amount.BigInt()
			return err
		})
		return BalanceOfMethod.Outputs.Pack(balance)
	case string(TransferMethod.ID):
		if readonly {
			return nil, errors.New("the method is not readonly")
		}
		args, err := TransferMethod.Inputs.Unpack(contract.Input[4:])
		if err != nil {
			return nil, errors.New("fail to unpack input arguments")
		}
		sender := args[0].(common.Address)
		recipient := args[1].(common.Address)
		amount := args[2].(*big.Int)
		if amount.Sign() <= 0 {
			return nil, errors.New("invalid amount")
		}
		from := sdk.AccAddress(sender.Bytes())
		to := sdk.AccAddress(recipient.Bytes())
		if err := bc.checkBlockedAddr(to); err != nil {
			return nil, err
		}
		denom := EVMDenom(contract.CallerAddress)
		amt := sdk.NewCoin(denom, sdkmath.NewIntFromBigInt(amount))
		err = stateDB.ExecuteNativeAction(precompileAddr, nil, func(ctx sdk.Context) error {
			if err := bc.bankKeeper.IsSendEnabledCoins(ctx, amt); err != nil {
				return err
			}
			if err := bc.bankKeeper.SendCoins(ctx, from, to, sdk.NewCoins(amt)); err != nil {
				return errorsmod.Wrap(err, "fail to send coins in precompiled contract")
			}
			return nil
		})
		return nil, err
	default:
		return nil, errors.New("unknown method")
	}
	return nil, nil
}
