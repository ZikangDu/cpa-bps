package adapter

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strings"
)

//go:embed manager/*
var managerResources embed.FS

var accountPage = template.Must(template.ParseFS(managerResources, "manager/index.html"))

type accountPageData struct {
	OAuthReady       bool
	DelegatedGateway bool
	BPSLogin         bool
}

const resourcePrefix = "/v0/resource/plugins/" + provider + "/"

func registerManagement() any {
	return map[string]any{"Routes": []map[string]string{
		{"Method": "GET", "Path": "/cpa-bps/concurrency"},
		{"Method": "POST", "Path": "/cpa-bps/concurrency"},
		{"Method": "GET", "Path": "/cpa-bps/quota"},
		{"Method": "POST", "Path": "/cpa-bps/reset"},
		{"Method": "GET", "Path": "/cpa-bps/plan-observations"},
		{"Method": "GET", "Path": "/cpa-bps/quality/config"},
		{"Method": "PUT", "Path": "/cpa-bps/quality/config"},
		{"Method": "POST", "Path": "/cpa-bps/quality/run"},
		{"Method": "GET", "Path": "/cpa-bps/quality/results"},
		{"Method": "GET", "Path": "/cpa-bps/quality/models"},
	}, "Resources": []map[string]string{
		{"Path": "index.html", "Menu": "BPS 账号授权", "Description": "View the gateway connection or authorize a registered OAuth client."},
		{"Path": "auth.css"},
		{"Path": "auth.js"},
	}}
}

type managementRequest struct {
	Method         string
	Path           string
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (r *Runtime) handleManagement(raw json.RawMessage) (any, error) {
	var req managementRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	if req.Path == "/v0/management/cpa-bps/concurrency" {
		response, err := r.manageConcurrency(req)
		if detail, ok := err.(*rpcError); ok {
			return bpsQuotaHTTP(detail.StatusCode(), map[string]string{"error": detail.Code()})
		}
		return response, err
	}
	if req.Path == "/v0/management/cpa-bps/quota" {
		return r.manageBPSQuota(req)
	}
	if req.Path == "/v0/management/cpa-bps/reset" {
		return r.manageBPSReset(req)
	}
	if req.Path == "/v0/management/cpa-bps/plan-observations" {
		return r.manageBPSPlans(req)
	}
	if strings.HasPrefix(req.Path, "/v0/management/cpa-bps/quality/") {
		return r.manageQuality(req)
	}
	resource := strings.TrimPrefix(req.Path, resourcePrefix)
	if req.Method != http.MethodGet || resource == req.Path {
		return map[string]int{"StatusCode": http.StatusNotFound}, nil
	}
	var contentType string
	switch resource {
	case "index.html":
		contentType = "text/html; charset=utf-8"
	case "auth.css":
		contentType = "text/css; charset=utf-8"
	case "auth.js":
		contentType = "application/javascript; charset=utf-8"
	default:
		return map[string]int{"StatusCode": http.StatusNotFound}, nil
	}
	var body []byte
	var err error
	if resource == "index.html" {
		var rendered bytes.Buffer
		err = accountPage.Execute(&rendered, r.accountPageData())
		body = rendered.Bytes()
	} else {
		body, err = managerResources.ReadFile("manager/" + resource)
	}
	if err != nil {
		return nil, failure(http.StatusInternalServerError, "bps_resource_unavailable", "the plugin resource is unavailable")
	}
	headers := http.Header{
		"Content-Type":           {contentType},
		"Cache-Control":          {"no-store"},
		"Referrer-Policy":        {"no-referrer"},
		"X-Content-Type-Options": {"nosniff"},
	}
	if resource == "index.html" {
		headers.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
	}
	return map[string]any{"StatusCode": http.StatusOK, "Headers": headers, "Body": body}, nil
}

func (r *Runtime) accountPageData() accountPageData {
	r.mu.Lock()
	cfg, available := r.cfg, r.cfg.Enabled && !r.closed
	r.mu.Unlock()
	return accountPageData{
		DelegatedGateway: available && cfg.DelegatedGateway,
		BPSLogin:         available && cfg.OAuthPreset == "bps",
		OAuthReady: available && cfg.OAuth != nil &&
			(cfg.OAuth.ClientSecretEnv == "" || strings.TrimSpace(os.Getenv(cfg.OAuth.ClientSecretEnv)) != ""),
	}
}
