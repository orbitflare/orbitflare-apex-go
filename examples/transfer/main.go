package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/gagliardetto/solana-go/programs/system"

	apex "github.com/orbitflare/orbitflare-apex-go"
	"github.com/orbitflare/orbitflare-apex-go/examples/internal/common"
)

func main() {
	common.Main(func(ctx context.Context) error {
		to, err := solana.PublicKeyFromBase58(os.Getenv("TO"))
		if err != nil {
			return fmt.Errorf("TO must be a base58 wallet address: %w", err)
		}
		lamports, err := strconv.ParseUint(os.Getenv("LAMPORTS"), 10, 64)
		if err != nil || lamports == 0 {
			return errors.New("LAMPORTS must be a positive number")
		}
		s, err := common.Load(ctx)
		if err != nil {
			return err
		}
		tipAccount, _ := apex.PickTipAccount(s.TipAccounts)
		blockhash, err := s.Solana.LatestBlockhash(ctx)
		if err != nil {
			return err
		}
		transfer := system.NewTransferInstruction(lamports, s.Payer.PublicKey(), to).Build()
		tip := apex.TipInstruction(s.Payer.PublicKey(), tipAccount, s.TipLamports)

		var tx *solana.Transaction
		if s.V1 {
			config := solana.TransactionConfig{}.WithComputeUnitLimit(50_000).WithLoadedAccountsDataSizeLimit(1024 * 1024).WithPriorityFee(4_000)
			tx, err = solana.NewTransaction([]solana.Instruction{transfer, tip}, blockhash,
				solana.TransactionPayer(s.Payer.PublicKey()), solana.TransactionV1Config(config))
		} else {
			tx, err = solana.NewTransaction([]solana.Instruction{
				computebudget.NewSetComputeUnitLimitInstruction(50_000).Build(),
				computebudget.NewSetComputeUnitPriceInstruction(apex.DefaultComputeUnitPriceMicroLamports).Build(),
				transfer,
				tip,
			}, blockhash, solana.TransactionPayer(s.Payer.PublicKey()))
		}
		if err != nil {
			return err
		}
		if err := s.Sign(tx); err != nil {
			return err
		}
		wire, err := apex.SerializeTransaction(tx)
		if err != nil {
			return err
		}
		version := "legacy"
		if s.V1 {
			version = "v1"
		}
		fmt.Printf("%s tx, %d bytes, %d lamports from %s to %s, tip %d to %s\n",
			version, len(wire), lamports, s.Payer.PublicKey(), to, s.TipLamports, tipAccount)

		client, err := apex.ConnectWithOptions(ctx, s.Options(), s.APIKey)
		if err != nil {
			return err
		}
		defer client.Close()
		sentAt := time.Now()
		signature, err := client.SendTransactionWithResponse(ctx, tx)
		if err != nil {
			return err
		}
		fmt.Printf("accepted in %d us: %s\n", time.Since(sentAt).Microseconds(), signature)
		return s.Report(ctx, signature.String(), sentAt)
	})
}
