package common

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"

	apex "github.com/orbitflare/orbitflare-apex-go"
	"github.com/orbitflare/orbitflare-apex-go/rpc"
)

var MemoProgram = solana.MustPublicKeyFromBase58("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr")

type Setup struct {
	APIKey      string
	Region      apex.Region
	QUIC        string
	RPCURL      string
	Payer       solana.PrivateKey
	Solana      *rpc.SolanaRPC
	TipLamports uint64
	TipAccounts []solana.PublicKey
	V1          bool
}

func env(name string) string {
	return os.Getenv(name)
}

func envUint(name string, fallback uint64) (uint64, error) {
	v := env(name)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

func Load(ctx context.Context) (*Setup, error) {
	s := &Setup{APIKey: env("APEX_API_KEY"), Region: apex.Frankfurt}
	if s.APIKey == "" {
		return nil, errors.New("APEX_API_KEY is required")
	}
	if r := env("APEX_REGION"); r != "" {
		region, ok := apex.ParseRegion(r)
		if !ok {
			return nil, fmt.Errorf("unknown APEX_REGION %s", r)
		}
		s.Region = region
	}
	s.QUIC = env("APEX_QUIC")
	if s.QUIC == "" {
		s.QUIC = s.Region.QUICEndpoint()
	}
	s.RPCURL = env("APEX_RPC")
	if s.RPCURL == "" {
		s.RPCURL = s.Region.RPCURL()
	}
	keypairPath := env("KEYPAIR_PATH")
	if keypairPath == "" {
		keypairPath = "payer.json"
	}
	payer, err := solana.PrivateKeyFromSolanaKeygenFile(keypairPath)
	if err != nil {
		return nil, fmt.Errorf("read keypair %s: %w", keypairPath, err)
	}
	s.Payer = payer
	solanaURL := env("SOLANA_RPC_URL")
	if solanaURL == "" {
		solanaURL = "https://api.mainnet-beta.solana.com"
	}
	s.Solana = rpc.NewSolanaRPC(solanaURL)
	if s.TipLamports, err = envUint("TIP_LAMPORTS", apex.MinTipLamports); err != nil {
		return nil, err
	}
	s.TipAccounts, err = rpc.NewWithURL(s.RPCURL, s.APIKey).GetTipAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("getTipAccounts: %w", err)
	}
	if len(s.TipAccounts) == 0 {
		return nil, errors.New("the endpoint published no tip accounts")
	}
	s.V1 = strings.EqualFold(env("APEX_TX_VERSION"), "v1")
	return s, nil
}

func (s *Setup) Options() apex.Options {
	return apex.Options{Endpoint: s.QUIC}
}

func (s *Setup) Memo(text string) solana.Instruction {
	return solana.NewInstruction(MemoProgram, solana.AccountMetaSlice{solana.Meta(s.Payer.PublicKey()).WRITE().SIGNER()}, []byte(text))
}

func (s *Setup) Sign(tx *solana.Transaction) error {
	_, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(s.Payer.PublicKey()) {
			return &s.Payer
		}
		return nil
	})
	return err
}

func (s *Setup) TippedMemo(ctx context.Context, label string) (*solana.Transaction, error) {
	blockhash, err := s.Solana.LatestBlockhash(ctx)
	if err != nil {
		return nil, err
	}
	return s.TippedMemoWith(label, blockhash)
}

func (s *Setup) TippedMemoWith(label string, blockhash solana.Hash) (*solana.Transaction, error) {
	pad, err := envUint("APEX_MEMO_BYTES", 0)
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("%s %d ", label, time.Now().UnixNano())
	if extra := int(pad) - len(text); extra > 0 {
		text += strings.Repeat("x", extra)
	}
	defaultLimit := uint64(100_000)
	if s.V1 {
		defaultLimit = 400_000
	}
	budget, err := envUint("APEX_CU_LIMIT", defaultLimit)
	if err != nil {
		return nil, err
	}
	tipAccount, _ := apex.PickTipAccount(s.TipAccounts)
	memo := s.Memo(text)
	tip := apex.TipInstruction(s.Payer.PublicKey(), tipAccount, s.TipLamports)

	var tx *solana.Transaction
	if s.V1 {
		config := solana.TransactionConfig{}.
			WithComputeUnitLimit(uint32(budget)).
			WithLoadedAccountsDataSizeLimit(1024 * 1024).
			WithPriorityFee(4_000)
		tx, err = solana.NewTransaction([]solana.Instruction{memo, tip}, blockhash,
			solana.TransactionPayer(s.Payer.PublicKey()), solana.TransactionV1Config(config))
	} else {
		tx, err = solana.NewTransaction([]solana.Instruction{
			computebudget.NewSetComputeUnitLimitInstruction(uint32(budget)).Build(),
			computebudget.NewSetComputeUnitPriceInstruction(apex.DefaultComputeUnitPriceMicroLamports).Build(),
			memo,
			tip,
		}, blockhash, solana.TransactionPayer(s.Payer.PublicKey()))
	}
	if err != nil {
		return nil, err
	}
	if err := s.Sign(tx); err != nil {
		return nil, err
	}
	if s.V1 {
		wire, err := apex.SerializeTransaction(tx)
		if err != nil {
			return nil, err
		}
		fmt.Printf("built a v1 transaction of %d bytes\n", len(wire))
	}
	return tx, nil
}

func (s *Setup) Report(ctx context.Context, signature string, sentAt time.Time) error {
	slot, landed, err := s.Solana.Confirm(ctx, signature, 30*time.Second)
	if err != nil {
		return err
	}
	if landed {
		fmt.Printf("landed in slot %d after %d ms: %s\n", slot, time.Since(sentAt).Milliseconds(), signature)
	} else {
		fmt.Printf("not confirmed within 30 s: %s\n", signature)
	}
	return nil
}

func Main(run func(ctx context.Context) error) {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
