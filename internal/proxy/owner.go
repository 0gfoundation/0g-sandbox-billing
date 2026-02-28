package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0gfoundation/0g-sandbox-billing/internal/daytona"
)

const ownerLabel = "daytona-owner"

// CheckOwner fetches sandbox metadata and verifies the owner label matches walletAddr.
func CheckOwner(ctx context.Context, dtona *daytona.Client, sandboxID, walletAddr string) error {
	sb, err := dtona.GetSandbox(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("get sandbox: %w", err)
	}
	owner, ok := sb.Labels[ownerLabel]
	if !ok || !strings.EqualFold(owner, walletAddr) {
		return fmt.Errorf("forbidden")
	}
	return nil
}

// InjectOwner sets labels["daytona-owner"] = walletAddr in the request body,
// and forces autostop and autoarchive intervals to 0.
func InjectOwner(body []byte, walletAddr string) ([]byte, error) {
	var m map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("unmarshal body: %w", err)
		}
	} else {
		m = make(map[string]any)
	}

	// Inject owner label
	labels, _ := m["labels"].(map[string]any)
	if labels == nil {
		labels = make(map[string]any)
	}
	labels[ownerLabel] = walletAddr
	m["labels"] = labels

	// Force autostop / autoarchive to 0 (billing proxy owns shutdown)
	m["autostopInterval"] = 0
	m["autoarchiveInterval"] = 0

	return json.Marshal(m)
}

// StripOwnerLabel removes the daytona-owner key from a label-update payload.
func StripOwnerLabel(body []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	delete(m, ownerLabel)
	return json.Marshal(m)
}
