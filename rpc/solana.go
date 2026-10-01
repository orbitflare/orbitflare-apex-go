package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gagliardetto/solana-go"
)

const (
	TipProgramID    = "9ig7pd4gqe2m16ACGPbPo4HfMGD3ba38poDhXEayx7EF"
	vaultTagBase64  = "QVBFWFZMVDE="
	vaultDataSize   = 41
	confirmInterval = 500 * time.Millisecond
)

func FetchVaults(ctx context.Context, solanaRPCURL string) ([]solana.PublicKey, error) {
	params := []any{TipProgramID, map[string]any{
		"encoding": "base64",
		"filters": []any{
			map[string]any{"dataSize": vaultDataSize},
			map[string]any{"memcmp": map[string]any{"offset": 0, "bytes": vaultTagBase64, "encoding": "base64"}},
		},
	}}
	raw, err := callJSONRPC(ctx, &http.Client{}, solanaRPCURL, "", "getProgramAccounts", params)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Pubkey string `json:"pubkey"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, ErrBadResponse
	}
	vaults := make([]solana.PublicKey, 0, len(rows))
	for _, row := range rows {
		if key, err := solana.PublicKeyFromBase58(row.Pubkey); err == nil {
			vaults = append(vaults, key)
		}
	}
	sort.Slice(vaults, func(i, j int) bool { return bytes.Compare(vaults[i][:], vaults[j][:]) < 0 })
	return vaults, nil
}

type SolanaRPC struct {
	http *http.Client
	url  string
}

func NewSolanaRPC(url string) *SolanaRPC {
	return &SolanaRPC{http: &http.Client{Timeout: 10 * time.Second}, url: url}
}

func (s *SolanaRPC) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return callJSONRPC(ctx, s.http, s.url, "", method, params)
}

func (s *SolanaRPC) LatestBlockhash(ctx context.Context) (solana.Hash, error) {
	raw, err := s.call(ctx, "getLatestBlockhash", []any{map[string]any{"commitment": "confirmed"}})
	if err != nil {
		return solana.Hash{}, err
	}
	var result struct {
		Value struct {
			Blockhash string `json:"blockhash"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return solana.Hash{}, ErrBadResponse
	}
	hash, err := solana.HashFromBase58(result.Value.Blockhash)
	if err != nil {
		return solana.Hash{}, ErrBadResponse
	}
	return hash, nil
}

func (s *SolanaRPC) Confirm(ctx context.Context, signature string, timeout time.Duration) (uint64, bool, error) {
	deadline := time.Now().Add(timeout)
	params := []any{[]string{signature}, map[string]any{"searchTransactionHistory": false}}
	for {
		raw, err := s.call(ctx, "getSignatureStatuses", params)
		if err != nil {
			return 0, false, err
		}
		var result struct {
			Value []*struct {
				Slot               *uint64         `json:"slot"`
				Err                json.RawMessage `json:"err"`
				ConfirmationStatus string          `json:"confirmationStatus"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return 0, false, ErrBadResponse
		}
		if len(result.Value) > 0 && result.Value[0] != nil {
			status := result.Value[0]
			if len(status.Err) > 0 && string(status.Err) != "null" {
				return 0, false, &Error{Code: 0, Message: fmt.Sprintf("transaction failed on chain: %s", status.Err)}
			}
			if status.ConfirmationStatus == "confirmed" || status.ConfirmationStatus == "finalized" {
				if status.Slot == nil {
					return 0, true, nil
				}
				return *status.Slot, true, nil
			}
		}
		if !time.Now().Before(deadline) {
			return 0, false, nil
		}
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-time.After(confirmInterval):
		}
	}
}
