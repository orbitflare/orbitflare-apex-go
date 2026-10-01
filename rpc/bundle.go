package rpc

import (
	"context"
	"encoding/json"
	"strings"
)

type BundleAccepted struct {
	BundleID   string
	Signatures []string
}

type BundleState int

const (
	BundlePending BundleState = iota
	BundleLanded
	BundleFailed
	BundleInvalid
)

func (s BundleState) String() string {
	switch s {
	case BundlePending:
		return "Pending"
	case BundleLanded:
		return "Landed"
	case BundleFailed:
		return "Failed"
	}
	return "Invalid"
}

type BundleStatus struct {
	BundleID   string
	State      BundleState
	LandedSlot *uint64
}

const MaxBundle = 4

func (c *Client) SendBundle(ctx context.Context, wires [][]byte) (BundleAccepted, error) {
	if len(wires) == 0 || len(wires) > MaxBundle {
		return BundleAccepted{}, ErrBadResponse
	}
	body, ok := encodeFrames(wires)
	if !ok {
		return BundleAccepted{}, ErrBadResponse
	}
	v, err := c.postBinary(ctx, strings.TrimRight(c.url, "/")+"/send-bundle", body)
	if err != nil {
		return BundleAccepted{}, err
	}
	bundleID, ok := v["bundle_id"].(string)
	if !ok {
		return BundleAccepted{}, plainError(v)
	}
	out := BundleAccepted{BundleID: bundleID}
	if list, ok := v["signatures"].([]any); ok {
		for _, s := range list {
			if sig, ok := s.(string); ok {
				out.Signatures = append(out.Signatures, sig)
			}
		}
	}
	return out, nil
}

func (c *Client) BundleStatuses(ctx context.Context, bundleIDs []string) ([]BundleStatus, error) {
	raw, err := c.call(ctx, "getInflightBundleStatuses", []any{bundleIDs})
	if err != nil {
		return nil, err
	}
	var result struct {
		Value []struct {
			BundleID   string  `json:"bundle_id"`
			Status     *string `json:"status"`
			LandedSlot *uint64 `json:"landed_slot"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Value == nil {
		return nil, ErrBadResponse
	}
	out := make([]BundleStatus, 0, len(result.Value))
	for _, s := range result.Value {
		state := BundleInvalid
		if s.Status != nil {
			switch *s.Status {
			case "Landed":
				state = BundleLanded
			case "Failed":
				state = BundleFailed
			case "Pending":
				state = BundlePending
			}
		}
		out = append(out, BundleStatus{BundleID: s.BundleID, State: state, LandedSlot: s.LandedSlot})
	}
	return out, nil
}
