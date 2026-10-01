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

		tx, err := s.TippedMemo(ctx, "apex raw_bytes")
		if err != nil {
			return err
		}
		signature := tx.Signatures[0]
		wire, err := apex.SerializeTransaction(tx)
		if err != nil {
			return err
		}
		packet := apex.EncodePacket(wire, false, nil)
		fmt.Printf("packet: %d bytes = 8 (len) + %d (tx) + 3 (flags)\n", len(packet), len(wire))
		fmt.Printf("first 16 bytes: % x\n", packet[:16])

		sentAt := time.Now()
		if err := client.SendTransactionBytes(ctx, wire); err != nil {
			return err
		}
		fmt.Printf("sent in %d us\n", time.Since(sentAt).Microseconds())
		return s.Report(ctx, signature.String(), sentAt)
	})
}
