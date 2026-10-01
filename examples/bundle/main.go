package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gagliardetto/solana-go"

	apex "github.com/orbitflare/orbitflare-apex-go"
	"github.com/orbitflare/orbitflare-apex-go/examples/internal/common"
	"github.com/orbitflare/orbitflare-apex-go/rpc"
)

func main() {
	common.Main(func(ctx context.Context) error {
		s, err := common.Load(ctx)
		if err != nil {
			return err
		}
		client := rpc.NewWithURL(s.RPCURL, s.APIKey)
		blockhash, err := s.Solana.LatestBlockhash(ctx)
		if err != nil {
			return err
		}

		first, err := solana.NewTransaction(
			[]solana.Instruction{s.Memo(fmt.Sprintf("apex bundle first %d", os.Getpid()))},
			blockhash, solana.TransactionPayer(s.Payer.PublicKey()))
		if err != nil {
			return err
		}
		if err := s.Sign(first); err != nil {
			return err
		}
		second, err := s.TippedMemoWith("apex bundle second", blockhash)
		if err != nil {
			return err
		}
		wires := make([][]byte, 0, 2)
		for _, tx := range []*solana.Transaction{first, second} {
			wire, err := apex.SerializeTransaction(tx)
			if err != nil {
				return err
			}
			wires = append(wires, wire)
		}

		started := time.Now()
		accepted, err := client.SendBundle(ctx, wires)
		if err != nil {
			return err
		}
		fmt.Printf("bundle %s accepted in %d us\n", accepted.BundleID, time.Since(started).Microseconds())

		for range 60 {
			statuses, err := client.BundleStatuses(ctx, []string{accepted.BundleID})
			if err != nil {
				return err
			}
			if len(statuses) > 0 {
				switch statuses[0].State {
				case rpc.BundleLanded:
					slot := "unknown"
					if statuses[0].LandedSlot != nil {
						slot = fmt.Sprint(*statuses[0].LandedSlot)
					}
					fmt.Printf("landed in slot %s after %d ms\n", slot, time.Since(started).Milliseconds())
					for _, sig := range accepted.Signatures {
						fmt.Printf("  %s\n", sig)
					}
					return nil
				case rpc.BundleFailed, rpc.BundleInvalid:
					fmt.Println("bundle did not land (no Jito leader before the blockhash expired)")
					return nil
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		fmt.Println("still pending after 30 s")
		return nil
	})
}
