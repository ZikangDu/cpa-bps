package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const (
	bpsModelAccessURL   = "https://bps.openai.com/basispoints/api/responses/access"
	maxBPSCatalogBytes  = 2 << 20
	maxBPSCatalogModels = 1024
)

// BPSModel is a model enabled for the authenticated BPS account.
type BPSModel struct {
	ID      string
	Label   string
	Efforts []string
}

// BPSModels reads the account's native BPS access catalog. The fixed query is
// added after endpoint validation, so generic caller-supplied URLs stay strict.
func (c *Client) BPSModels(ctx context.Context, token, accountID string) ([]BPSModel, error) {
	request, err := newRequest(ctx, http.MethodGet, bpsModelAccessURL, token, nil)
	if err != nil {
		return nil, err
	}
	if len(accountID) > 256 || strings.ContainsFunc(accountID, func(r rune) bool { return r <= ' ' || r >= 127 }) {
		return nil, ErrInvalidToken
	}
	if accountID != "" {
		request.Header.Set("ChatGPT-Account-ID", accountID)
	}
	request.URL.RawQuery = "include_models=true"
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrModelCatalog
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBPSCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBPSCatalogBytes {
		return nil, ErrModelCatalog
	}
	return parseBPSModels(raw)
}

func parseBPSModels(raw []byte) ([]BPSModel, error) {
	var access struct {
		Allowed *bool `json:"allowed"`
		Catalog *struct {
			Models     []json.RawMessage `json:"models"`
			Restricted []string          `json:"restricted_models"`
		} `json:"model_catalog"`
	}
	if json.Unmarshal(raw, &access) != nil || access.Allowed == nil {
		return nil, ErrModelCatalog
	}
	models := make([]BPSModel, 0)
	if !*access.Allowed {
		return models, nil
	}
	if access.Catalog == nil || access.Catalog.Models == nil ||
		len(access.Catalog.Models) > maxBPSCatalogModels || len(access.Catalog.Restricted) > maxBPSCatalogModels {
		return nil, ErrModelCatalog
	}
	excluded := make(map[string]bool, len(access.Catalog.Restricted))
	for _, id := range access.Catalog.Restricted {
		excluded[id] = true
	}
	for _, rawModel := range access.Catalog.Models {
		var entry struct {
			ID      string `json:"id"`
			Label   string `json:"label"`
			Enabled *bool  `json:"model_picker_enabled"`
			Policy  struct {
				State string `json:"state"`
			} `json:"policy"`
			Efforts []struct {
				Value string `json:"value"`
			} `json:"efforts"`
		}
		if json.Unmarshal(rawModel, &entry) != nil || !validBPSModelID(entry.ID) || excluded[entry.ID] ||
			(entry.Enabled != nil && !*entry.Enabled) || entry.Policy.State == "disabled" || entry.Policy.State == "unconfigured" {
			continue
		}
		excluded[entry.ID] = true
		label := strings.TrimSpace(entry.Label)
		if label == "" || len(label) > 256 || strings.ContainsFunc(label, func(r rune) bool { return r < ' ' || r == 127 }) {
			label = entry.ID
		}
		model := BPSModel{ID: entry.ID, Label: label}
		seenEffort := make(map[string]bool)
		for _, effort := range entry.Efforts {
			switch effort.Value {
			case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
				if !seenEffort[effort.Value] {
					model.Efforts = append(model.Efforts, effort.Value)
					seenEffort[effort.Value] = true
				}
			}
		}
		models = append(models, model)
	}
	return models, nil
}

func validBPSModelID(id string) bool {
	if id == "" || len(id) > 128 || strings.HasSuffix(id, "-excel") {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune("_.:/-", r)) {
			return false
		}
	}
	return true
}
