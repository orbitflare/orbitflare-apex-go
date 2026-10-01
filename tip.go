package apex

import (
	"math/rand/v2"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
)

const (
	MinTipLamports                       uint64 = 1_000_000
	DefaultComputeUnitPriceMicroLamports uint64 = 10_000
)

func TipInstruction(payer, tipAccount solana.PublicKey, lamports uint64) solana.Instruction {
	return system.NewTransferInstruction(lamports, payer, tipAccount).Build()
}

func PickTipAccount(tipAccounts []solana.PublicKey) (solana.PublicKey, bool) {
	if len(tipAccounts) == 0 {
		return solana.PublicKey{}, false
	}
	return tipAccounts[rand.IntN(len(tipAccounts))], true
}

func SerializeTransaction(tx *solana.Transaction) ([]byte, error) {
	if tx == nil {
		return nil, wrap(ErrSerialize, errNilTransaction)
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return nil, wrap(ErrSerialize, err)
	}
	return wire, nil
}

func RetryBudget(n uint16) *uint16 {
	return &n
}
