package main

import (
	"context"
	"fmt"
	"time"

	apex "github.com/orbitflare/orbitflare-apex-go"
	"github.com/orbitflare/orbitflare-apex-go/examples/internal/common"
)

func main() {
	common.Main(func(ctx context.Context) error {
		s, err := common.Load(ctx)
		if err != nil {
			return err
		}
		client, err := apex.ConnectWithOptions(ctx, s.Options(), s.APIKey)
		if err != nil {
			return err
		}
		defer client.Close()
		fmt.Printf("connected to %s (%s)\n", client.RemoteAddr(), s.Region.Code())

		tx, err := s.TippedMemo(ctx, "apex quic_send")
		if err != nil {
			return err
		}
		sentAt := time.Now()
		signature, err := client.SendTransaction(ctx, tx)
		if err != nil {
			return err
		}
		fmt.Printf("sent in %d us\n", time.Since(sentAt).Microseconds())
		return s.Report(ctx, signature.String(), sentAt)
	})
}
