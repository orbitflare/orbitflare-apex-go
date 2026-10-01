package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"

	apex "github.com/orbitflare/orbitflare-apex-go"
	"github.com/orbitflare/orbitflare-apex-go/examples/internal/common"
)

type result struct {
	signature solana.Signature
	elapsed   time.Duration
	err       error
}

func main() {
	common.Main(func(ctx context.Context) error {
		count := 10
		if len(os.Args) > 1 {
			n, err := strconv.Atoi(os.Args[1])
			if err != nil || n < 1 {
				return fmt.Errorf("count must be a positive number, got %q", os.Args[1])
			}
			count = n
		}
		s, err := common.Load(ctx)
		if err != nil {
			return err
		}
		client, err := apex.ConnectWithOptions(ctx, s.Options(), s.APIKey)
		if err != nil {
			return err
		}
		defer client.Close()

		blockhash, err := s.Solana.LatestBlockhash(ctx)
		if err != nil {
			return err
		}
		txs := make([]*solana.Transaction, count)
		for i := range txs {
			if txs[i], err = s.TippedMemoWith(fmt.Sprintf("apex throughput %d", i), blockhash); err != nil {
				return err
			}
		}

		started := time.Now()
		results := make([]result, count)
		var wg sync.WaitGroup
		for i, tx := range txs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				t := time.Now()
				sig, err := client.SendTransaction(ctx, tx)
				results[i] = result{sig, time.Since(t), err}
			}()
		}
		wg.Wait()

		latencies := make([]int64, 0, count)
		for _, r := range results {
			if r.err != nil {
				return r.err
			}
			latencies = append(latencies, r.elapsed.Microseconds())
		}
		slices.Sort(latencies)
		fmt.Printf("%d sends in %d ms: per-send p50 %d us, p99 %d us, reconnects %d\n",
			count, time.Since(started).Milliseconds(), latencies[count/2],
			latencies[min(count*99/100, count-1)], client.ReconnectsTotal())

		landed := 0
		for _, r := range results {
			_, ok, err := s.Solana.Confirm(ctx, r.signature.String(), 30*time.Second)
			if err != nil {
				return err
			}
			if ok {
				landed++
			}
		}
		fmt.Printf("landed %d/%d within 30 s\n", landed, count)
		return nil
	})
}
