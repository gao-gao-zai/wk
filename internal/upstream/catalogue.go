package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

var errReadSchema = errors.New("unsupported upstream read response")

// Fall back only for an unavailable API or a known incompatible schema.
// Authentication, rate limits, proxy failures and cancellation keep their
// existing classification rather than silently triggering additional calls.
func legacyReadAllowed(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, ErrNoProxyNode) {
		return false
	}
	if errors.Is(err, errReadSchema) {
		return true
	}
	var e *Error
	return errors.As(err, &e) && (e.Status == 404 || e.Status == 405 || e.Status == 410 || e.Status == 501)
}

type catalogueModel struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Tags            []string `json:"tags"`
	Disabled        bool     `json:"disabled"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	Reasoning       struct {
		SupportedEfforts []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

type modelCatalogue struct {
	rows    []ModelInfo
	blocked map[string]bool
}

func (c *Client) FetchModelsContext(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	// Pin identity and credentials for every request in this collection.
	account := c.growthAuthSnapshot(a, a.Snapshot())
	primary, err := c.readCatalogue(ctx, account, false)
	if err != nil && legacyReadAllowed(ctx, err) {
		rows, legacyErr := c.fetchLegacyModels(ctx, account)
		if legacyErr == nil {
			primary.rows = rows
			err = nil
		} else if !legacyReadAllowed(ctx, legacyErr) {
			return nil, legacyErr
		}
	}
	if err != nil && !legacyReadAllowed(ctx, err) {
		return nil, err
	}
	if account.EnterpriseID != "" {
		extra, enterpriseErr := c.readCatalogue(ctx, account, true)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if enterpriseErr == nil {
			seen := make(map[string]bool, len(primary.rows))
			for _, m := range primary.rows {
				seen[m.ID] = true
			}
			for _, m := range extra.rows {
				if !seen[m.ID] && !primary.blocked[m.ID] {
					primary.rows = append(primary.rows, m)
					seen[m.ID] = true
				}
			}
		} else if len(primary.rows) == 0 {
			return nil, enterpriseErr
		}
	}
	if len(primary.rows) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("models api returned empty list")
	}
	cache := make(map[string][]string)
	for _, row := range primary.rows {
		if len(row.Efforts) != 0 {
			cache[row.ID] = append([]string(nil), row.Efforts...)
		}
	}
	c.storeCapabilities(account, cache)
	return primary.rows, nil
}

func (c *Client) readCatalogue(ctx context.Context, a *auth.Auth, enterprise bool) (modelCatalogue, error) {
	result := modelCatalogue{blocked: make(map[string]bool)}
	path := "/v3/config"
	if enterprise {
		path = "/console/enterprises/" + url.PathEscape(a.EnterpriseID) + "/config/models"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.chatBase(a)+path, nil)
	if err != nil {
		return result, err
	}
	if c.desktopHeaders() {
		c.commonHeaders(req, a)
	} else {
		accountHeaders(req, a)
		req.Header.Set("User-Agent", clientUA)
	}
	req.Header.Set("Accept", "application/json")
	data, err := c.doJSON(req, a)
	if err != nil {
		return result, err
	}
	var models []catalogueModel
	var cliIDs []string
	if enterprise {
		if json.Unmarshal(data, &models) != nil || models == nil {
			return result, errReadSchema
		}
	} else {
		var payload struct {
			Models []catalogueModel `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		}
		if json.Unmarshal(data, &payload) != nil || payload.Models == nil {
			return result, errReadSchema
		}
		models = payload.Models
		for _, agent := range payload.Agents {
			if agent.Name == "cli" {
				cliIDs = agent.Models
				break
			}
		}
	}
	allowed := make(map[string]bool)
	for _, id := range cliIDs {
		allowed[NormalizeModelID(id)] = true
	}
	seen := make(map[string]bool)
	for _, m := range models {
		id := NormalizeModelID(m.ID)
		chat := !enterprise
		for _, tag := range m.Tags {
			if tag == "chat" {
				chat = true
			}
			if tag == "text-to-image" || tag == "image-to-image" || tag == "text-to-video" || tag == "image-to-video" {
				m.Disabled = true
			}
		}
		if m.Disabled || strings.HasPrefix(id, "nes-") || strings.HasPrefix(id, "completion-") || strings.HasPrefix(id, "codewise-") || (m.MaxOutputTokens > 0 && m.MaxOutputTokens <= 256) {
			result.blocked[id] = true
			continue
		}
		if id == "" || !chat || seen[id] || (len(cliIDs) != 0 && !allowed[id]) {
			continue
		}
		seen[id] = true
		result.rows = append(result.rows, ModelInfo{ID: id, Name: m.Name, ContextWindow: m.MaxInputTokens, MaxTokens: m.MaxOutputTokens, Efforts: m.Reasoning.SupportedEfforts})
	}
	return result, nil
}
