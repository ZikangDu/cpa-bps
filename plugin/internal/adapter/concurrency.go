package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxAccountConcurrency = 10000
const maxConcurrencyPolicyBytes = 1 << 20

var errConcurrencyPolicyUnavailable = errors.New("BPS concurrency policy is unavailable")

// concurrencyPolicies serializes policy reads with atomic updates and queue
// changes, so a concurrent execution cannot restore a stale capacity.
type concurrencyPolicies struct {
	mu sync.Mutex
}

type concurrencyPolicy struct {
	Version int            `json:"version"`
	Limits  map[string]int `json:"limits"`
}

func newConcurrencyPolicies() *concurrencyPolicies {
	return &concurrencyPolicies{}
}

func concurrencyPolicyPath(cfg config) string {
	if cfg.ConcurrencyPolicyPath != "" {
		return cfg.ConcurrencyPolicyPath
	}
	return filepath.Join("plugins", "cpa-bps-concurrency.json")
}

func (r *Runtime) accountLimit(cfg config, authID string) (int, error) {
	r.concurrency.mu.Lock()
	defer r.concurrency.mu.Unlock()
	policy, err := readConcurrencyPolicy(concurrencyPolicyPath(cfg))
	if err != nil {
		return 0, errConcurrencyPolicyUnavailable
	}
	limit := cfg.ConcurrencyLimit
	if saved, exists := policy.Limits[authID]; exists {
		limit = saved
	}
	if err := r.queue.SetLimit(authID, limit); err != nil {
		return 0, errConcurrencyPolicyUnavailable
	}
	return limit, nil
}

func (r *Runtime) manageConcurrency(req managementRequest) (any, error) {
	values := req.Query["auth_index"]
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" || len(values[0]) > 256 {
		return nil, failure(http.StatusBadRequest, "bps_invalid_auth_index", "one credential index is required")
	}
	if req.Method != http.MethodGet && req.Method != http.MethodPost {
		return nil, failure(http.StatusMethodNotAllowed, "bps_unsupported_method", "use GET or POST for account concurrency")
	}
	var requestedLimit *int
	if req.Method == http.MethodPost {
		var update struct {
			Limit *int `json:"limit"`
		}
		if len(req.Body) > 1024 || decodePolicyJSON(req.Body, &update) != nil ||
			update.Limit == nil || *update.Limit < 0 || *update.Limit > maxAccountConcurrency {
			return nil, failure(http.StatusBadRequest, "bps_invalid_concurrency_limit", "concurrency limit must be an integer from 0 to 10000")
		}
		requestedLimit = update.Limit
	}
	cfg, ok := r.begin()
	if !ok {
		return nil, failure(http.StatusServiceUnavailable, "bps_plugin_unavailable", "BPS plugin is unavailable")
	}
	defer r.tasks.Done()
	if r.host == nil {
		return nil, failure(http.StatusServiceUnavailable, "bps_credential_unavailable", "the credential directory is unavailable")
	}
	var listing struct {
		Files []struct {
			ID        string `json:"id"`
			AuthIndex string `json:"auth_index"`
			Provider  string `json:"provider"`
			Type      string `json:"type"`
		} `json:"files"`
	}
	err := r.host("host.auth.list", map[string]any{"host_callback_id": req.HostCallbackID}, &listing)
	if err != nil {
		return nil, failure(http.StatusServiceUnavailable, "bps_credential_unavailable", "the credential directory is unavailable")
	}
	authID := ""
	for _, entry := range listing.Files {
		if entry.AuthIndex == values[0] && (entry.Provider == provider || entry.Type == provider) &&
			entry.ID != "" && len(entry.ID) <= 1024 {
			if authID != "" {
				return nil, failure(http.StatusConflict, "bps_ambiguous_auth_index", "the credential index is ambiguous")
			}
			authID = entry.ID
		}
	}
	if authID == "" {
		return nil, failure(http.StatusNotFound, "bps_credential_unavailable", "the selected BPS credential is unavailable")
	}

	r.concurrency.mu.Lock()
	defer r.concurrency.mu.Unlock()
	path := concurrencyPolicyPath(cfg)
	policy, err := readConcurrencyPolicy(path)
	if err != nil {
		return nil, failure(http.StatusServiceUnavailable, "bps_concurrency_policy_unavailable", errConcurrencyPolicyUnavailable.Error())
	}
	limit := cfg.ConcurrencyLimit
	if saved, exists := policy.Limits[authID]; exists {
		limit = saved
	}
	if requestedLimit != nil {
		limit = *requestedLimit
		if saved, exists := policy.Limits[authID]; !exists || saved != limit {
			policy.Limits[authID] = limit
			if err := writeConcurrencyPolicy(path, policy); err != nil {
				return nil, failure(http.StatusServiceUnavailable, "bps_concurrency_policy_unavailable", "the concurrency limit could not be saved")
			}
		}
	}
	if err := r.queue.SetLimit(authID, limit); err != nil {
		return nil, failure(http.StatusServiceUnavailable, "bps_concurrency_policy_unavailable", errConcurrencyPolicyUnavailable.Error())
	}
	snapshot := r.queue.Snapshot(authID, limit)
	body, err := json.Marshal(struct {
		Active  int    `json:"active"`
		Waiting int    `json:"waiting"`
		Limit   int    `json:"limit"`
		Scope   string `json:"scope"`
	}{snapshot.Active, snapshot.Waiting, snapshot.Limit, "plugin_instance"})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers":    http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
		"Body":       body,
	}, nil
}

func readConcurrencyPolicy(path string) (concurrencyPolicy, error) {
	policy := concurrencyPolicy{Version: 1, Limits: make(map[string]int)}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return policy, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxConcurrencyPolicyBytes {
		return policy, errConcurrencyPolicyUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return policy, errConcurrencyPolicyUnavailable
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxConcurrencyPolicyBytes+1))
	if err != nil || len(raw) > maxConcurrencyPolicyBytes || decodePolicyJSON(raw, &policy) != nil ||
		policy.Version != 1 || policy.Limits == nil || len(policy.Limits) > 10000 {
		return concurrencyPolicy{}, errConcurrencyPolicyUnavailable
	}
	for id, limit := range policy.Limits {
		if id == "" || len(id) > 1024 || limit < 0 || limit > maxAccountConcurrency {
			return concurrencyPolicy{}, errConcurrencyPolicyUnavailable
		}
	}
	return policy, nil
}

func writeConcurrencyPolicy(path string, policy concurrencyPolicy) error {
	if policy.Version != 1 || policy.Limits == nil || len(policy.Limits) > 10000 {
		return errConcurrencyPolicyUnavailable
	}
	raw, err := json.MarshalIndent(policy, "", "  ")
	if err != nil || len(raw) >= maxConcurrencyPolicyBytes {
		return errConcurrencyPolicyUnavailable
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".cpa-bps-concurrency-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(raw, '\n')); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

func decodePolicyJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}
