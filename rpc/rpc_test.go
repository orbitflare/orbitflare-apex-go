package rpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apex "github.com/orbitflare/orbitflare-apex-go"
)

type seen struct {
	method, path, query, apiKey, contentType string
	body                                     []byte
}

func server(t *testing.T, handle func(s seen, w http.ResponseWriter)) (*httptest.Server, *seen) {
	t.Helper()
	last := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*last = seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("x-api-key"), r.Header.Get("Content-Type"), body}
		handle(*last, w)
	}))
	t.Cleanup(srv.Close)
	return srv, last
}

func jsonRPC(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBatchFramesAreLengthPrefixedBigEndian(t *testing.T) {
	b, ok := EncodeBatch([][]byte{{1, 2, 3}, {9}})
	if !ok || !bytes.Equal(b, []byte{0, 3, 1, 2, 3, 0, 1, 9}) {
		t.Fatalf("got %v", b)
	}
	if _, ok := EncodeBatch(nil); ok {
		t.Fatal("empty batch encoded")
	}
	tooMany := make([][]byte, MaxBatch+1)
	for i := range tooMany {
		tooMany[i] = []byte{1}
	}
	if _, ok := EncodeBatch(tooMany); ok {
		t.Fatal("oversized batch encoded")
	}
	if _, ok := EncodeBatch([][]byte{make([]byte, apex.MaxTransactionSize+1)}); ok {
		t.Fatal("oversized transaction encoded")
	}
	if _, ok := EncodeBatch([][]byte{{}}); ok {
		t.Fatal("empty transaction encoded")
	}
}

func TestGetTipAccounts(t *testing.T) {
	srv, last := server(t, func(s seen, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":["APeX2oLtjYehgTMUCA971L8htM7tGNqsXHDz5NrivhhX","APeXAKT6spXSmU3uRv2MoEgQ7ckhAS4nexTgaxwLEfJ9"]}`))
	})
	keys, err := NewWithURL(srv.URL, "key-1").GetTipAccounts(context.Background())
	if err != nil || len(keys) != 2 || keys[0].String() != "APeX2oLtjYehgTMUCA971L8htM7tGNqsXHDz5NrivhhX" {
		t.Fatalf("keys %v err %v", keys, err)
	}
	req := jsonRPC(t, last.body)
	if req["method"] != "getTipAccounts" || last.apiKey != "key-1" || last.method != http.MethodPost {
		t.Fatalf("request %+v", last)
	}
}

func TestSendTransactionJSONRPC(t *testing.T) {
	srv, last := server(t, func(s seen, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"5sig"}`))
	})
	sig, err := NewWithURL(srv.URL, "k").SendTransaction(context.Background(), []byte{1, 2, 3}, true, apex.RetryBudget(4))
	if err != nil || sig != "5sig" {
		t.Fatalf("sig %q err %v", sig, err)
	}
	req := jsonRPC(t, last.body)
	params := req["params"].([]any)
	if params[0] != base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) || params[2] != true {
		t.Fatalf("params %v", params)
	}
	config := params[1].(map[string]any)
	if config["encoding"] != "base64" || config["maxRetries"] != float64(4) {
		t.Fatalf("config %v", config)
	}

	srv2, last2 := server(t, func(s seen, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"no tip"}}`))
	})
	_, err = NewWithURL(srv2.URL, "k").SendTransaction(context.Background(), []byte{1}, false, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 || rpcErr.Message != "no tip" {
		t.Fatalf("got %v", err)
	}
	cfg := jsonRPC(t, last2.body)["params"].([]any)[1].(map[string]any)
	if _, ok := cfg["maxRetries"]; ok {
		t.Fatal("maxRetries sent without a budget")
	}
}

func TestSendTransactionBinary(t *testing.T) {
	srv, last := server(t, func(s seen, w http.ResponseWriter) {
		if s.path != "/send-bin" {
			w.WriteHeader(404)
			return
		}
		if bytes.Equal(s.body, []byte{0xbb}) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"below_floor","message":"tip 1000 below floor 1000000"}`))
			return
		}
		_, _ = w.Write([]byte(`{"signature":"3abc"}`))
	})
	c := NewWithURL(srv.URL+"/", "k")
	sig, err := c.SendTransactionBinary(context.Background(), []byte{0xaa, 0x01}, true, apex.RetryBudget(2))
	if err != nil || sig != "3abc" {
		t.Fatalf("sig %q err %v", sig, err)
	}
	if last.query != "mev_protect=1&max_retries=2" || last.contentType != "application/octet-stream" || last.apiKey != "k" || !bytes.Equal(last.body, []byte{0xaa, 0x01}) {
		t.Fatalf("request %+v", last)
	}
	_, err = c.SendTransactionBinary(context.Background(), []byte{0xbb}, false, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Message != "below_floor: tip 1000 below floor 1000000" {
		t.Fatalf("got %v", err)
	}
	if last.query != "mev_protect=0" {
		t.Fatalf("query %q", last.query)
	}
}

func TestSendBatch(t *testing.T) {
	srv, last := server(t, func(s seen, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"attempted":2,"accepted":1,"rejected":1,"results":[{"signature":"s1"},{"error":"no_tip","message":"missing tip"}]}`))
	})
	res, err := NewWithURL(srv.URL, "k").SendBatch(context.Background(), [][]byte{{1, 2}, {3}}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := BatchResult{Attempted: 2, Accepted: 1, Rejected: 1, Results: []BatchItem{{Accepted: true, Signature: "s1"}, {Error: "no_tip", Message: "missing tip"}}}
	if res.Attempted != want.Attempted || res.Accepted != want.Accepted || res.Rejected != want.Rejected || len(res.Results) != 2 || res.Results[0] != want.Results[0] || res.Results[1] != want.Results[1] {
		t.Fatalf("got %+v", res)
	}
	if last.path != "/send-batch" || !bytes.Equal(last.body, []byte{0, 2, 1, 2, 0, 1, 3}) {
		t.Fatalf("request %+v", last)
	}
	if _, err := NewWithURL(srv.URL, "k").SendBatch(context.Background(), nil, false, nil); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("empty batch: %v", err)
	}
}

func TestPing(t *testing.T) {
	srv, last := server(t, func(s seen, w http.ResponseWriter) {
		_, _ = w.Write([]byte("pong"))
	})
	if err := NewWithURL(srv.URL, "").Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last.method != http.MethodGet || last.path != "/ping" || last.apiKey != "" {
		t.Fatalf("request %+v", last)
	}
	bad, _ := server(t, func(s seen, w http.ResponseWriter) { _, _ = w.Write([]byte("nope")) })
	if err := NewWithURL(bad.URL, "").Ping(context.Background()); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("got %v", err)
	}
	down, _ := server(t, func(s seen, w http.ResponseWriter) { w.WriteHeader(503) })
	if err := NewWithURL(down.URL, "").Ping(context.Background()); err == nil {
		t.Fatal("a 503 ping succeeded")
	}
}

func TestBundles(t *testing.T) {
	srv, last := server(t, func(s seen, w http.ResponseWriter) {
		switch s.path {
		case "/send-bundle":
			_, _ = w.Write([]byte(`{"bundle_id":"b1","signatures":["x","y"]}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":[{"bundle_id":"b1","status":"Landed","landed_slot":42},{"bundle_id":"b2","status":"Pending","landed_slot":null},{"bundle_id":"b3","status":"Failed"},{"bundle_id":"b4","status":"Invalid"}]}}`))
		}
	})
	c := NewWithURL(srv.URL, "k")
	accepted, err := c.SendBundle(context.Background(), [][]byte{{1}, {2, 3}})
	if err != nil || accepted.BundleID != "b1" || strings.Join(accepted.Signatures, ",") != "x,y" {
		t.Fatalf("accepted %+v err %v", accepted, err)
	}
	if !bytes.Equal(last.body, []byte{0, 1, 1, 0, 2, 2, 3}) || last.contentType != "application/octet-stream" {
		t.Fatalf("request %+v", last)
	}
	statuses, err := c.BundleStatuses(context.Background(), []string{"b1", "b2", "b3", "b4"})
	if err != nil || len(statuses) != 4 {
		t.Fatalf("statuses %+v err %v", statuses, err)
	}
	if statuses[0].State != BundleLanded || statuses[0].LandedSlot == nil || *statuses[0].LandedSlot != 42 {
		t.Fatalf("first %+v", statuses[0])
	}
	if statuses[1].State != BundlePending || statuses[1].LandedSlot != nil || statuses[2].State != BundleFailed || statuses[3].State != BundleInvalid {
		t.Fatalf("states %+v", statuses)
	}
	params := jsonRPC(t, last.body)["params"].([]any)[0].([]any)
	if len(params) != 4 || params[0] != "b1" {
		t.Fatalf("params %v", params)
	}
	for _, bad := range [][][]byte{nil, {{1}, {1}, {1}, {1}, {1}}, {{}}, {make([]byte, apex.MaxTransactionSize+1)}} {
		if _, err := c.SendBundle(context.Background(), bad); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("bundle of %d accepted locally: %v", len(bad), err)
		}
	}
}

func TestFetchVaultsSortsByKeyBytes(t *testing.T) {
	srv, last := server(t, func(s seen, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"pubkey":"APeXn29deoxpsZz6r7n63Ymmv2skBr3WhKYLB1mB2fR7"},{"pubkey":"APeX2oLtjYehgTMUCA971L8htM7tGNqsXHDz5NrivhhX"},{"pubkey":"not-a-key"}]}`))
	})
	vaults, err := FetchVaults(context.Background(), srv.URL)
	if err != nil || len(vaults) != 2 {
		t.Fatalf("vaults %v err %v", vaults, err)
	}
	if bytes.Compare(vaults[0][:], vaults[1][:]) >= 0 {
		t.Fatal("vaults not sorted by key bytes")
	}
	req := jsonRPC(t, last.body)
	params := req["params"].([]any)
	filters := params[1].(map[string]any)["filters"].([]any)
	if req["method"] != "getProgramAccounts" || params[0] != TipProgramID || filters[0].(map[string]any)["dataSize"] != float64(41) || filters[1].(map[string]any)["memcmp"].(map[string]any)["bytes"] != "QVBFWFZMVDE=" {
		t.Fatalf("request %v", req)
	}
}

func TestSolanaRPCBlockhashAndConfirm(t *testing.T) {
	var polls atomic.Int32
	srv, _ := server(t, func(s seen, w http.ResponseWriter) {
		req := map[string]any{}
		_ = json.Unmarshal(s.body, &req)
		switch req["method"] {
		case "getLatestBlockhash":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":{"blockhash":"EETubP5AKHgjPAhzPAFcb8BAY1hMH639CWCFTqi3hq1k","lastValidBlockHeight":1}}}`))
		case "getSignatureStatuses":
			sig := req["params"].([]any)[0].([]any)[0]
			switch {
			case sig == "failed":
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":[{"slot":7,"err":{"InstructionError":[0,"Custom"]},"confirmationStatus":"processed"}]}}`))
			case sig == "never":
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":[null]}}`))
			case polls.Add(1) < 3:
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":[{"slot":99,"err":null,"confirmationStatus":"processed"}]}}`))
			default:
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":[{"slot":99,"err":null,"confirmationStatus":"confirmed"}]}}`))
			}
		}
	})
	s := NewSolanaRPC(srv.URL)
	hash, err := s.LatestBlockhash(context.Background())
	if err != nil || hash.String() != "EETubP5AKHgjPAhzPAFcb8BAY1hMH639CWCFTqi3hq1k" {
		t.Fatalf("hash %v err %v", hash, err)
	}
	slot, ok, err := s.Confirm(context.Background(), "good", 10*time.Second)
	if err != nil || !ok || slot != 99 || polls.Load() != 3 {
		t.Fatalf("slot %d ok %v err %v polls %d", slot, ok, err, polls.Load())
	}
	_, _, err = s.Confirm(context.Background(), "failed", time.Second)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || !strings.HasPrefix(rpcErr.Message, "transaction failed on chain:") {
		t.Fatalf("got %v", err)
	}
	start := time.Now()
	_, ok, err = s.Confirm(context.Background(), "never", 600*time.Millisecond)
	if err != nil || ok || time.Since(start) < 500*time.Millisecond {
		t.Fatalf("ok %v err %v after %v", ok, err, time.Since(start))
	}
}

func TestRegionURLs(t *testing.T) {
	if New(apex.Tokyo, "k").url != "http://tyo.apex.orbitflare.com" {
		t.Fatal(New(apex.Tokyo, "k").url)
	}
}
