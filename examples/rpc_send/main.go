package main

import (
	"context"
	"fmt"
	"time"

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
		tx, err := s.TippedMemo(ctx, "apex rpc_send")
		if err != nil {
			return err
		}
		wire, err := apex.SerializeTransaction(tx)
		if err != nil {
			return err
		}
		sentAt := time.Now()
		signature, err := client.SendTransaction(ctx, wire, false, nil)
		if err != nil {
			return err
		}
		fmt.Printf("accepted in %d us\n", time.Since(sentAt).Microseconds())
		return s.Report(ctx, signature, sentAt)
	})
}
