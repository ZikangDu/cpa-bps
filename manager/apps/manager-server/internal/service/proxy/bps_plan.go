package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpa"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

const (
	bpsPlanObservationPath = "/v0/management/cpa-bps/plan-observations"
	maxBPSPlanListBytes    = 4 << 20
	maxBPSObservationBytes = 1 << 20
	bpsPlanFreshness       = 5 * time.Minute
	bpsPlanTimeout         = 2 * time.Second
)

type bpsPlanObservation struct {
	AuthIndex  string `json:"auth_index"`
	PlanType   string `json:"plan_type"`
	ObservedAt string `json:"observed_at"`
	Source     string `json:"source"`
}

// The plugin supplies cached observations only. This optional enrichment never
// reads provider credentials or makes provider requests while listing accounts.
func enrichBPSAuthFilePlans(ctx context.Context, response *http.Response, setup store.Setup) {
	if response.StatusCode != http.StatusOK || response.Body == nil ||
		response.ContentLength > maxBPSPlanListBytes || response.Header.Get("Content-Encoding") != "" {
		return
	}
	originalBody := response.Body
	raw, err := io.ReadAll(io.LimitReader(originalBody, maxBPSPlanListBytes+1))
	response.Body = &prefixedResponseBody{Reader: io.MultiReader(bytes.NewReader(raw), originalBody), Closer: originalBody}
	if err != nil || len(raw) > maxBPSPlanListBytes {
		return
	}
	var document map[string]json.RawMessage
	var files []map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil || json.Unmarshal(document["files"], &files) != nil {
		return
	}
	indexCounts := make(map[string]int)
	hasBPS := false
	for _, file := range files {
		index := rawFieldString(file["auth_index"])
		if index != "" {
			indexCounts[index]++
		}
		hasBPS = hasBPS || isBPSAuthFile(file)
	}
	if !hasBPS {
		return
	}
	observations := fetchBPSPlanObservations(ctx, setup)
	if len(observations) == 0 {
		return
	}
	changed := false
	for _, file := range files {
		index := rawFieldString(file["auth_index"])
		observation, found := observations[index]
		if !found || indexCounts[index] != 1 || !isBPSAuthFile(file) {
			continue
		}
		file["plan_type"], _ = json.Marshal(observation.PlanType)
		file["plan_type_source"], _ = json.Marshal(observation.Source)
		file["plan_type_observed_at"], _ = json.Marshal(observation.ObservedAt)
		changed = true
	}
	if !changed {
		return
	}
	document["files"], err = json.Marshal(files)
	if err != nil {
		return
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return
	}
	_ = originalBody.Close()
	response.Body = io.NopCloser(bytes.NewReader(encoded))
	response.ContentLength = int64(len(encoded))
	response.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
	for _, header := range []string{"ETag", "Content-MD5", "Digest"} {
		response.Header.Del(header)
	}
}

type prefixedResponseBody struct {
	io.Reader
	io.Closer
}

func fetchBPSPlanObservations(ctx context.Context, setup store.Setup) map[string]bpsPlanObservation {
	ctx, cancel := context.WithTimeout(ctx, bpsPlanTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		cpa.NormalizeBaseURL(setup.CPAUpstreamURL)+bpsPlanObservationPath, nil)
	if err != nil {
		return nil
	}
	request.Header.Set("Authorization", "Bearer "+setup.ManagementKey)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{Timeout: bpsPlanTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBPSObservationBytes+1))
	if err != nil || len(raw) > maxBPSObservationBytes {
		return nil
	}
	return parseBPSPlanObservations(raw, time.Now())
}

func parseBPSPlanObservations(raw []byte, now time.Time) map[string]bpsPlanObservation {
	var payload struct {
		Accounts []bpsPlanObservation `json:"accounts"`
	}
	if json.Unmarshal(raw, &payload) != nil || len(payload.Accounts) > 10000 {
		return nil
	}
	observations := make(map[string]bpsPlanObservation)
	counts := make(map[string]int)
	for _, entry := range payload.Accounts {
		counts[entry.AuthIndex]++
	}
	for _, entry := range payload.Accounts {
		if counts[entry.AuthIndex] != 1 || entry.AuthIndex == "" || len(entry.AuthIndex) > 256 ||
			strings.ContainsFunc(entry.AuthIndex, func(r rune) bool { return r <= ' ' || r >= 127 }) ||
			entry.Source != "active_usage" || !knownBPSPlan(entry.PlanType) {
			continue
		}
		observed, err := time.Parse(time.RFC3339Nano, entry.ObservedAt)
		if err != nil || observed.After(now) || now.Sub(observed) > bpsPlanFreshness {
			continue
		}
		entry.ObservedAt = observed.UTC().Format(time.RFC3339Nano)
		observations[entry.AuthIndex] = entry
	}
	return observations
}

func knownBPSPlan(plan string) bool {
	switch plan {
	case "free", "go", "plus", "pro", "team", "business", "enterprise", "edu":
		return true
	default:
		return false
	}
}

func isBPSAuthFile(file map[string]json.RawMessage) bool {
	provider := rawFieldString(file["provider"])
	if provider == "" {
		provider = rawFieldString(file["type"])
	}
	return strings.EqualFold(strings.TrimSpace(provider), "cpa-bps")
}

func rawFieldString(value json.RawMessage) string {
	var text string
	_ = json.Unmarshal(value, &text)
	return text
}
