package main

import (
	"context"
	"errors"
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

		tx, err := s.TippedMemo(ctx, "apex quic_send_with_response")
		if err != nil {
			return err
		}
		sentAt := time.Now()
		signature, err := client.SendTransactionWithResponse(ctx, tx)
		var rejected *apex.RejectedError
		switch {
		case errors.As(err, &rejected):
			fmt.Printf("rejected: %s: %s\n", rejected.Code, rejected.Message)
			return nil
		case err != nil:
			return err
		}
		fmt.Printf("accepted in %d us\n", time.Since(sentAt).Microseconds())
		return s.Report(ctx, signature.String(), sentAt)
	})
}
