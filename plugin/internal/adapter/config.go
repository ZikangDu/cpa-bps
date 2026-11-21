package adapter

import (
	"fmt"
	"net/url"
	"strings"

	"cpa-bps-reconstruction/internal/oauthflow"
	"cpa-bps-reconstruction/internal/upstream"
	"gopkg.in/yaml.v3"
)

type modelConfig struct {
	Name  string `yaml:"name"`
	Alias string `yaml:"alias"`
}

func (m *modelConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		m.Name = node.Value
		return nil
	}
	type fields modelConfig
	return node.Decode((*fields)(m))
}

type config struct {
	Enabled                  bool              `yaml:"enabled"`
	ResponsesURL             string            `yaml:"responses_url"`
	ModelsURL                string            `yaml:"models_url"`
	FilesURL                 string            `yaml:"files_url"`
	QuotaURL                 string            `yaml:"quota_url"`
	OAuth                    *oauthflow.Config `yaml:"oauth"`
	OAuthPreset              string            `yaml:"oauth_preset"`
	OAuthProxyURL            string            `yaml:"oauth_proxy_url"`
	DelegatedGateway         bool              `yaml:"delegated_gateway"`
	OmitEncryptedHistory     bool              `yaml:"omit_encrypted_history"`
	Models                   []modelConfig     `yaml:"models"`
	AutoDiscover             bool              `yaml:"auto_discover"`
	ModelSuffix              string            `yaml:"model_suffix"`
	FastMode                 bool              `yaml:"fast_mode"`
	ConcurrencyLimit         int               `yaml:"account_concurrency_limit"`
	ConcurrencyPolicyPath    string            `yaml:"concurrency_policy_path"`
	QualityStatePath         string            `yaml:"quality_state_path"`
	BPSToolTransport         string            `yaml:"bps_tool_transport"`
	BPSToolsVersionID        string            `yaml:"bps_tools_version_id"`
	BPSCompactThreshold      int               `yaml:"bps_compact_threshold"`
	NativeToolRepairAttempts int               `yaml:"native_tool_repair_attempts"`
	DocumentWorkerPath       string            `yaml:"document_worker_path"`
	DocumentPython           string            `yaml:"document_python"`
}

func parseConfig(raw []byte) (config, error) {
	cfg := config{AutoDiscover: true, ModelSuffix: "-excel", ConcurrencyLimit: 20, NativeToolRepairAttempts: 2}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return config{}, fmt.Errorf("plugin configuration is invalid YAML")
	}
	if !cfg.Enabled {
		return cfg, nil
	}
	if cfg.DocumentWorkerPath == "" {
		cfg.DocumentWorkerPath = "plugins/bps-document-worker"
	}
	if cfg.DocumentPython == "" {
		cfg.DocumentPython = "plugins/document-runtime/bin/python"
	}
	if cfg.BPSToolTransport == "" {
		cfg.BPSToolTransport = "auto"
	}
	if cfg.BPSToolTransport != "auto" && cfg.BPSToolTransport != "text" && cfg.BPSToolTransport != "native" {
		return config{}, fmt.Errorf("bps_tool_transport must be auto, text or native")
	}
	if len(cfg.BPSToolsVersionID) > 256 || strings.ContainsFunc(cfg.BPSToolsVersionID, func(r rune) bool { return r <= ' ' || r == 127 }) {
		return config{}, fmt.Errorf("bps_tools_version_id must be a bounded identifier")
	}
	if cfg.BPSCompactThreshold < 0 || cfg.BPSCompactThreshold > 2000000 {
		return config{}, fmt.Errorf("bps_compact_threshold must be between 0 and 2000000")
	}
	if cfg.NativeToolRepairAttempts < 0 || cfg.NativeToolRepairAttempts > 2 {
		return config{}, fmt.Errorf("native_tool_repair_attempts must be between 0 and 2")
	}
	switch cfg.OAuthPreset {
	case "":
	case "bps":
		if cfg.OAuth != nil || cfg.DelegatedGateway {
			return config{}, fmt.Errorf("oauth_preset bps requires an independent BPS connection")
		}
		oauth := oauthflow.BPSConfig()
		oauth.ProxyURL = cfg.OAuthProxyURL
		cfg.OAuth = &oauth
		if cfg.ResponsesURL == "" {
			cfg.ResponsesURL = oauthflow.BPSResponsesURL
		}
		if cfg.ResponsesURL != oauthflow.BPSResponsesURL {
			return config{}, fmt.Errorf("oauth_preset bps requires the BPS Responses endpoint")
		}
	default:
		return config{}, fmt.Errorf("oauth_preset must be bps or empty")
	}
	if cfg.OAuthProxyURL != "" && cfg.OAuthPreset == "" {
		return config{}, fmt.Errorf("oauth_proxy_url is used with oauth_preset bps")
	}
	if err := upstream.ValidateEndpoint(cfg.ResponsesURL); err != nil {
		return config{}, fmt.Errorf("invalid responses_url: %w", err)
	}
	if cfg.DelegatedGateway && cfg.OAuth != nil {
		return config{}, fmt.Errorf("delegated_gateway requires the upstream to manage OAuth")
	}
	for _, endpoint := range []struct{ name, value string }{
		{"models_url", cfg.ModelsURL}, {"files_url", cfg.FilesURL}, {"quota_url", cfg.QuotaURL},
	} {
		if endpoint.value == "" {
			continue
		}
		if err := upstream.ValidateEndpoint(endpoint.value); err != nil {
			return config{}, fmt.Errorf("invalid %s: %w", endpoint.name, err)
		}
		if !sameOrigin(cfg.ResponsesURL, endpoint.value) {
			return config{}, fmt.Errorf("%s and responses_url must have the same origin", endpoint.name)
		}
	}
	if cfg.OAuth != nil {
		if err := cfg.OAuth.Validate(); err != nil {
			return config{}, fmt.Errorf("invalid oauth configuration: %w", err)
		}
		cfg.OAuth.ResourceOrigin = endpointOrigin(cfg.ResponsesURL)
	}
	if cfg.ConcurrencyLimit < 0 || cfg.ConcurrencyLimit > 10000 {
		return config{}, fmt.Errorf("account_concurrency_limit must be between 0 and 10000")
	}
	if cfg.ModelSuffix == "无后缀" {
		cfg.ModelSuffix = ""
	}
	if cfg.ModelSuffix != "" && cfg.ModelSuffix != "-excel" {
		return config{}, fmt.Errorf("model_suffix must be -excel or empty")
	}
	aliases := make(map[string]bool, len(cfg.Models))
	for index := range cfg.Models {
		model := &cfg.Models[index]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" {
			return config{}, fmt.Errorf("a model entry is missing its upstream name")
		}
		if model.Alias == "" {
			model.Alias = model.Name
		}
		if aliases[model.Alias] {
			return config{}, fmt.Errorf("model aliases must be unique")
		}
		aliases[model.Alias] = true
	}
	if len(cfg.Models) == 0 && !cfg.discoveryEnabled() {
		return config{}, fmt.Errorf("configure models or set models_url with auto_discover enabled")
	}
	return cfg, nil
}

func (c config) discoveryEnabled() bool {
	return len(c.Models) == 0 && c.AutoDiscover && (c.ModelsURL != "" || c.OAuthPreset == "bps")
}

func sameOrigin(left, right string) bool {
	first, firstErr := url.Parse(left)
	second, secondErr := url.Parse(right)
	if firstErr != nil || secondErr != nil || first.Host == "" || second.Host == "" {
		return false
	}
	return first.Scheme == second.Scheme &&
		strings.EqualFold(first.Hostname(), second.Hostname()) && originPort(first) == originPort(second)
}

func originPort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	if value.Scheme == "https" {
		return "443"
	}
	return "80"
}

func endpointOrigin(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func (c config) upstreamModel(alias string) string {
	for _, model := range c.Models {
		if model.Alias == alias {
			return model.Name
		}
	}
	if c.discoveryEnabled() && strings.HasSuffix(alias, c.ModelSuffix) {
		return strings.TrimSuffix(alias, c.ModelSuffix)
	}
	return ""
}
