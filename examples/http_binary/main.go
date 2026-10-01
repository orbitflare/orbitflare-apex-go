package main

import (
	"context"
	"fmt"
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
		if err := client.Ping(ctx); err != nil {
			return err
		}

		tx, err := s.TippedMemo(ctx, "apex http_binary")
		if err != nil {
			return err
		}
		wire, err := apex.SerializeTransaction(tx)
		if err != nil {
			return err
		}
		sentAt := time.Now()
		signature, err := client.SendTransactionBinary(ctx, wire, false, nil)
		if err != nil {
			return err
		}
		fmt.Printf("send-bin accepted in %d us\n", time.Since(sentAt).Microseconds())
		if err := s.Report(ctx, signature, sentAt); err != nil {
			return err
		}

		blockhash, err := s.Solana.LatestBlockhash(ctx)
		if err != nil {
			return err
		}
		batch := make([][]byte, 0, 3)
		var signatures []solana.Signature
		for i := range 3 {
			tx, err := s.TippedMemoWith(fmt.Sprintf("apex http_binary batch %d", i), blockhash)
			if err != nil {
				return err
			}
			wire, err := apex.SerializeTransaction(tx)
			if err != nil {
				return err
			}
			batch = append(batch, wire)
			signatures = append(signatures, tx.Signatures[0])
		}
		sentAt = time.Now()
		result, err := client.SendBatch(ctx, batch, false, nil)
		if err != nil {
			return err
		}
		fmt.Printf("batch: %d attempted, %d accepted, %d rejected in %d us\n",
			result.Attempted, result.Accepted, result.Rejected, time.Since(sentAt).Microseconds())
		for i, item := range result.Results {
			if !item.Accepted {
				fmt.Printf("  %d rejected: %s: %s\n", i, item.Error, item.Message)
				continue
			}
			if err := s.Report(ctx, signatures[i].String(), sentAt); err != nil {
				return err
			}
		}
		return nil
	})
}
