package rpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"

	apex "github.com/orbitflare/orbitflare-apex-go"
)

var ErrBadResponse = errors.New("apex rpc: bad response")

type Error struct {
	Code    int64
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("apex rpc: rpc error %d: %s", e.Code, e.Message)
}

type Client struct {
	http   *http.Client
	url    string
	apiKey string
}

func New(region apex.Region, apiKey string) *Client {
	return NewWithURL(region.RPCURL(), apiKey)
}

func NewWithURL(url, apiKey string) *Client {
	return &Client{
		http:   &http.Client{Timeout: 5 * time.Second},
		url:    url,
		apiKey: apiKey,
	}
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int64  `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func callJSONRPC(ctx context.Context, client *http.Client, url, apiKey, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("apex rpc: decode response: %w", err)
	}
	if out.Error != nil {
		return nil, &Error{Code: out.Error.Code, Message: out.Error.Message}
	}
	if len(out.Result) == 0 {
		return nil, ErrBadResponse
	}
	return out.Result, nil
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return callJSONRPC(ctx, c.http, c.url, c.apiKey, method, params)
}

func (c *Client) GetTipAccounts(ctx context.Context) ([]solana.PublicKey, error) {
	raw, err := c.call(ctx, "getTipAccounts", []any{})
	if err != nil {
		return nil, err
	}
	var list []any
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, ErrBadResponse
	}
	keys := make([]solana.PublicKey, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			continue
		}
		key, err := solana.PublicKeyFromBase58(s)
		if err != nil {
			return nil, ErrBadResponse
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func (c *Client) SendTransaction(ctx context.Context, wire []byte, mevProtect bool, maxRetries *uint16) (string, error) {
	config := map[string]any{"encoding": "base64"}
	if maxRetries != nil {
		config["maxRetries"] = *maxRetries
	}
	raw, err := c.call(ctx, "sendTransaction", []any{base64.StdEncoding.EncodeToString(wire), config, mevProtect})
	if err != nil {
		return "", err
	}
	var signature string
	if err := json.Unmarshal(raw, &signature); err != nil {
		return "", ErrBadResponse
	}
	return signature, nil
}

func (c *Client) plainURL(route string, mevProtect bool, maxRetries *uint16) string {
	mev := 0
	if mevProtect {
		mev = 1
	}
	url := fmt.Sprintf("%s%s?mev_protect=%d", strings.TrimRight(c.url, "/"), route, mev)
	if maxRetries != nil {
		url += fmt.Sprintf("&max_retries=%d", *maxRetries)
	}
	return url
}

func plainError(v map[string]any) error {
	errName, _ := v["error"].(string)
	if errName == "" {
		errName = "error"
	}
	message, _ := v["message"].(string)
	return &Error{Code: 0, Message: errName + ": " + message}
}

func (c *Client) postBinary(ctx context.Context, url string, body []byte) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("apex rpc: decode response: %w", err)
	}
	return v, nil
}

func (c *Client) SendTransactionBinary(ctx context.Context, wire []byte, mevProtect bool, maxRetries *uint16) (string, error) {
	v, err := c.postBinary(ctx, c.plainURL("/send-bin", mevProtect, maxRetries), wire)
	if err != nil {
		return "", err
	}
	if signature, ok := v["signature"].(string); ok {
		return signature, nil
	}
	return "", plainError(v)
}

type BatchItem struct {
	Accepted  bool
	Signature string
	Error     string
	Message   string
}

type BatchResult struct {
	Attempted int
	Accepted  int
	Rejected  int
	Results   []BatchItem
}

const MaxBatch = 16

func EncodeBatch(wires [][]byte) ([]byte, bool) {
	if len(wires) == 0 || len(wires) > MaxBatch {
		return nil, false
	}
	return encodeFrames(wires)
}

func encodeFrames(wires [][]byte) ([]byte, bool) {
	size := 0
	for _, w := range wires {
		if len(w) == 0 || len(w) > apex.MaxTransactionSize {
			return nil, false
		}
		size += len(w) + 2
	}
	out := make([]byte, 0, size)
	for _, w := range wires {
		out = append(out, byte(len(w)>>8), byte(len(w)))
		out = append(out, w...)
	}
	return out, true
}

func asCount(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func (c *Client) SendBatch(ctx context.Context, wires [][]byte, mevProtect bool, maxRetries *uint16) (BatchResult, error) {
	body, ok := EncodeBatch(wires)
	if !ok {
		return BatchResult{}, ErrBadResponse
	}
	v, err := c.postBinary(ctx, c.plainURL("/send-batch", mevProtect, maxRetries), body)
	if err != nil {
		return BatchResult{}, err
	}
	results, ok := v["results"].([]any)
	if !ok {
		return BatchResult{}, plainError(v)
	}
	out := BatchResult{
		Attempted: asCount(v["attempted"]),
		Accepted:  asCount(v["accepted"]),
		Rejected:  asCount(v["rejected"]),
		Results:   make([]BatchItem, 0, len(results)),
	}
	for _, r := range results {
		row, _ := r.(map[string]any)
		if signature, ok := row["signature"].(string); ok {
			out.Results = append(out.Results, BatchItem{Accepted: true, Signature: signature})
			continue
		}
		errName, _ := row["error"].(string)
		if errName == "" {
			errName = "error"
		}
		message, _ := row["message"].(string)
		out.Results = append(out.Results, BatchItem{Error: errName, Message: message})
	}
	return out, nil
}

func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.url, "/")+"/ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("apex rpc: ping: http %d", resp.StatusCode)
	}
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if string(text) != "pong" {
		return ErrBadResponse
	}
	return nil
}

func FetchTipAccounts(ctx context.Context, region apex.Region, apiKey string) ([]solana.PublicKey, error) {
	return New(region, apiKey).GetTipAccounts(ctx)
}
