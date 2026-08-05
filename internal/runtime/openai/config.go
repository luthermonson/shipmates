package openai

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Defaults for every bound this runtime enforces. All are overridable through
// the settings map; none may be disabled by setting them to zero (zero means
// "use the default"), because an unbounded response from an untrusted
// endpoint is how a demo turns into an OOM.
const (
	// DefaultTimeout bounds a whole turn, not a single read. GPU clouds
	// queue; a long first-token wait is normal, an infinite one is not.
	DefaultTimeout = 10 * time.Minute
	// DefaultMaxResponseBytes caps the total bytes read from one streamed
	// response body.
	DefaultMaxResponseBytes int64 = 8 << 20 // 8 MiB
	// DefaultMaxLineBytes caps a single SSE line. Real chunks are a few
	// hundred bytes; a megabyte line is a server bug or an attack.
	DefaultMaxLineBytes = 256 << 10 // 256 KiB
	// DefaultMaxTranscriptBytes caps the in-memory transcript per session.
	DefaultMaxTranscriptBytes = 512 << 10 // 512 KiB
	// DefaultMaxTranscriptMessages caps transcript length in messages.
	DefaultMaxTranscriptMessages = 200
	// DefaultMaxPromptBytes caps one turn's input text.
	DefaultMaxPromptBytes = 256 << 10 // 256 KiB
	// DefaultMaxSystemPromptBytes caps persona prompt + memory digest.
	DefaultMaxSystemPromptBytes = 64 << 10 // 64 KiB
	// maxMalformedChunks is how many undecodable SSE payloads we tolerate in
	// one response before giving up on the server. Not configurable: a
	// server that emits this much garbage is broken, not slow.
	maxMalformedChunks = 16
	// maxErrorBodyBytes caps how much of a non-2xx body we read to build an
	// error message.
	maxErrorBodyBytes = 64 << 10
)

// Config is the resolved configuration for one OpenAI-compatible endpoint.
//
// It is populated by [ParseConfig] from the settings map that
// internal/runtime/config hands the factory (runtime.Factory.New's
// `settings map[string]any`), so there is no second config file and no
// parallel loader. Keys are snake_case, matching the YAML the operator
// writes:
//
//	base_url                 (required) e.g. https://inference.internal/v1
//	model                    (required) e.g. moonshotai/Kimi-K2-Instruct
//	api_key_env              name of the env var holding the key; unset = no auth
//	organization             sent as OpenAI-Organization
//	timeout                  duration string ("5m") or seconds (300)
//	headers                  map of extra request headers
//	ca_file                  PEM file appended to the system trust store
//	allow_plaintext_http     permit http:// to a non-loopback host (no key)
//	temperature              float, omitted from the request when unset
//	max_tokens               int, omitted from the request when unset
//	max_response_bytes       int, see DefaultMaxResponseBytes
//	max_line_bytes           int, see DefaultMaxLineBytes
//	max_transcript_bytes     int, see DefaultMaxTranscriptBytes
//	max_transcript_messages  int, see DefaultMaxTranscriptMessages
//	max_prompt_bytes         int, see DefaultMaxPromptBytes
//	max_system_prompt_bytes  int, see DefaultMaxSystemPromptBytes
//
// The API key itself is deliberately absent. Config never holds it; see
// [Config.apiKey].
type Config struct {
	BaseURL      string
	Model        string
	APIKeyEnv    string
	Organization string
	Timeout      time.Duration
	Headers      map[string]string
	CAFile       string
	// AllowPlaintextHTTP permits an http:// base URL whose host is not
	// loopback. Never permits sending a key in cleartext — that is refused
	// regardless of this flag.
	AllowPlaintextHTTP bool
	// Temperature is a pointer so "unset" stays out of the request body.
	// Some servers reject a temperature they do not implement.
	Temperature *float64
	MaxTokens   int

	MaxResponseBytes      int64
	MaxLineBytes          int
	MaxTranscriptBytes    int
	MaxTranscriptMessages int
	MaxPromptBytes        int
	MaxSystemPromptBytes  int
}

// metadataKeys are factory-level keys that may appear in a runtime's settings
// block without being ours. Tolerated so a wrapper can pass its block
// through unmodified; everything else unknown is an error, because a silently
// ignored typo in base_url or api_key_env is a support ticket.
var metadataKeys = map[string]bool{
	"runtime": true, "name": true, "type": true, "backend": true,
}

// rejectedKeys are keys we refuse loudly rather than ignore. Two families:
// a secret pasted into config, and any spelling of "turn off certificate
// verification". Both are mistakes worth stopping a boot for.
var rejectedKeys = map[string]string{
	"api_key":              "put the key in an environment variable and name it with api_key_env; shipmates never reads a key from config",
	"apikey":               "put the key in an environment variable and name it with api_key_env; shipmates never reads a key from config",
	"api-key":              "put the key in an environment variable and name it with api_key_env; shipmates never reads a key from config",
	"key":                  "put the key in an environment variable and name it with api_key_env; shipmates never reads a key from config",
	"token":                "put the token in an environment variable and name it with api_key_env; shipmates never reads a key from config",
	"secret":               "put the secret in an environment variable and name it with api_key_env; shipmates never reads a key from config",
	"insecure_skip_verify": "TLS verification is not optional; for a private CA use ca_file, or install the CA in the system trust store",
	"insecure":             "TLS verification is not optional; for a private CA use ca_file, or install the CA in the system trust store",
	"skip_tls_verify":      "TLS verification is not optional; for a private CA use ca_file, or install the CA in the system trust store",
	"tls_insecure":         "TLS verification is not optional; for a private CA use ca_file, or install the CA in the system trust store",
	"verify_ssl":           "TLS verification is not optional; for a private CA use ca_file, or install the CA in the system trust store",
}

// ParseConfig turns a runtime settings map into a validated Config.
//
// Values are type-tolerant on purpose: the same map may arrive from YAML
// (int, float64, bool, string) or JSON (float64 for every number), and
// operators write timeouts both as "90s" and as 90.
func ParseConfig(settings map[string]any) (Config, error) {
	cfg := Config{
		Timeout:               DefaultTimeout,
		MaxResponseBytes:      DefaultMaxResponseBytes,
		MaxLineBytes:          DefaultMaxLineBytes,
		MaxTranscriptBytes:    DefaultMaxTranscriptBytes,
		MaxTranscriptMessages: DefaultMaxTranscriptMessages,
		MaxPromptBytes:        DefaultMaxPromptBytes,
		MaxSystemPromptBytes:  DefaultMaxSystemPromptBytes,
	}

	known := map[string]bool{}
	mark := func(k string) { known[k] = true }

	for k := range settings {
		if reason, bad := rejectedKeys[strings.ToLower(strings.TrimSpace(k))]; bad {
			return Config{}, fmt.Errorf("openai runtime: setting %q is not supported: %s", k, reason)
		}
	}

	var err error
	if cfg.BaseURL, err = optString(settings, "base_url"); err != nil {
		return Config{}, err
	}
	mark("base_url")
	if cfg.Model, err = optString(settings, "model"); err != nil {
		return Config{}, err
	}
	mark("model")
	if cfg.APIKeyEnv, err = optString(settings, "api_key_env"); err != nil {
		return Config{}, err
	}
	mark("api_key_env")
	if cfg.Organization, err = optString(settings, "organization"); err != nil {
		return Config{}, err
	}
	mark("organization")
	if cfg.CAFile, err = optString(settings, "ca_file"); err != nil {
		return Config{}, err
	}
	mark("ca_file")
	if cfg.AllowPlaintextHTTP, err = optBool(settings, "allow_plaintext_http"); err != nil {
		return Config{}, err
	}
	mark("allow_plaintext_http")
	if d, ok, derr := optDuration(settings, "timeout"); derr != nil {
		return Config{}, derr
	} else if ok {
		cfg.Timeout = d
	}
	mark("timeout")
	if cfg.Headers, err = optHeaders(settings, "headers"); err != nil {
		return Config{}, err
	}
	mark("headers")
	if f, ok, ferr := optFloat(settings, "temperature"); ferr != nil {
		return Config{}, ferr
	} else if ok {
		cfg.Temperature = &f
	}
	mark("temperature")

	intFields := []struct {
		key string
		set func(int64)
	}{
		{"max_tokens", func(v int64) { cfg.MaxTokens = int(v) }},
		{"max_response_bytes", func(v int64) { cfg.MaxResponseBytes = v }},
		{"max_line_bytes", func(v int64) { cfg.MaxLineBytes = int(v) }},
		{"max_transcript_bytes", func(v int64) { cfg.MaxTranscriptBytes = int(v) }},
		{"max_transcript_messages", func(v int64) { cfg.MaxTranscriptMessages = int(v) }},
		{"max_prompt_bytes", func(v int64) { cfg.MaxPromptBytes = int(v) }},
		{"max_system_prompt_bytes", func(v int64) { cfg.MaxSystemPromptBytes = int(v) }},
	}
	for _, f := range intFields {
		v, ok, ierr := optInt(settings, f.key)
		if ierr != nil {
			return Config{}, ierr
		}
		if ok {
			if v <= 0 {
				return Config{}, fmt.Errorf("openai runtime: %s must be positive, got %d", f.key, v)
			}
			f.set(v)
		}
		mark(f.key)
	}

	var unknown []string
	for k := range settings {
		lk := strings.ToLower(strings.TrimSpace(k))
		if known[lk] || metadataKeys[lk] {
			continue
		}
		unknown = append(unknown, k)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Config{}, fmt.Errorf("openai runtime: unknown setting(s): %s", strings.Join(unknown, ", "))
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks the invariants a live client depends on. Called by
// ParseConfig; exported so a caller that builds Config by hand (tests, an
// embedder) gets the same checks.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.BaseURL) == "" {
		return fmt.Errorf("openai runtime: base_url is required (e.g. https://inference.internal/v1)")
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("openai runtime: model is required")
	}
	u, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil {
		return fmt.Errorf("openai runtime: base_url is not a URL: %w", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			if c.APIKeyEnv != "" {
				return fmt.Errorf("openai runtime: base_url %q is plaintext http to a non-loopback host and api_key_env is set; a bearer token must not travel in cleartext — use https", u.Redacted())
			}
			if !c.AllowPlaintextHTTP {
				return fmt.Errorf("openai runtime: base_url %q is plaintext http to a non-loopback host; use https, or set allow_plaintext_http: true if the network is genuinely trusted", u.Redacted())
			}
		}
	default:
		return fmt.Errorf("openai runtime: base_url scheme %q unsupported (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("openai runtime: base_url %q has no host", c.BaseURL)
	}
	if u.User != nil {
		return fmt.Errorf("openai runtime: base_url must not embed credentials; use api_key_env")
	}
	for k := range c.Headers {
		if strings.EqualFold(k, "authorization") {
			return fmt.Errorf("openai runtime: headers must not set Authorization; name the env var with api_key_env instead")
		}
	}
	if c.CAFile != "" {
		if _, err := loadCAPool(c.CAFile); err != nil {
			return err
		}
	}
	if c.Timeout < 0 {
		return fmt.Errorf("openai runtime: timeout must not be negative")
	}
	if c.MaxLineBytes < 512 {
		return fmt.Errorf("openai runtime: max_line_bytes must be at least 512, got %d", c.MaxLineBytes)
	}
	return nil
}

// Endpoint is the chat-completions URL derived from base_url. base_url is
// expected to include the API version prefix (".../v1"), matching how every
// OpenAI-compatible server documents itself; an already-complete endpoint is
// accepted as-is so an operator who pasted the full path is not punished.
func (c *Config) Endpoint() string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

// ModelsEndpoint is base_url + /models. Used only by [Runtime.Probe] for a
// cheap reachability check; no turn depends on it.
func (c *Config) ModelsEndpoint() string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	base = strings.TrimSuffix(base, "/chat/completions")
	return strings.TrimRight(base, "/") + "/models"
}

// apiKey reads the credential from the environment at call time.
//
// Read per request, not cached on Config, for three reasons: the key never
// sits in a struct that might get logged with %+v, a rotated env var is
// picked up without a restart, and "configured but empty" can be reported as
// the distinct, actionable error it is.
//
// An empty APIKeyEnv means the endpoint needs no auth — the normal case for a
// self-hosted server on a private network — and no Authorization header is
// sent at all.
func (c *Config) apiKey() (string, error) {
	if c.APIKeyEnv == "" {
		return "", nil
	}
	v := strings.TrimSpace(os.Getenv(c.APIKeyEnv))
	if v == "" {
		return "", fmt.Errorf("openai runtime: api_key_env names %q but that environment variable is unset or empty", c.APIKeyEnv)
	}
	return v, nil
}

// httpClient builds the transport. Cloned from http.DefaultTransport so
// proxy settings, HTTP/2, and connection pooling behave as the operator's
// environment expects.
//
// There is no code path here that can disable certificate verification.
// MinVersion is pinned up, never down; a private CA is added to a pool that
// starts from the system store so public endpoints keep working.
func (c *Config) httpClient() (*http.Client, error) {
	tr, _ := http.DefaultTransport.(*http.Transport)
	if tr == nil {
		return nil, fmt.Errorf("openai runtime: unexpected default transport type")
	}
	tr = tr.Clone()
	tlsCfg := tr.TLSClientConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	} else {
		tlsCfg = tlsCfg.Clone()
	}
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	if c.CAFile != "" {
		pool, err := loadCAPool(c.CAFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = pool
	}
	tr.TLSClientConfig = tlsCfg
	// No Client.Timeout: it would kill a long stream mid-answer. A whole-turn
	// deadline is applied to the request context instead (see Runtime.SendTurn).
	return &http.Client{Transport: tr}, nil
}

// loadCAPool returns the system trust store with the PEM at path appended.
// Additive, never replacing: an enterprise CA should not stop the same binary
// from reaching a public endpoint.
func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("openai runtime: ca_file %q: %w", path, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("openai runtime: ca_file %q contains no usable PEM certificate", path)
	}
	return pool, nil
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "localhost.localdomain":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// --- settings map accessors -------------------------------------------------

func lookup(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	// Tolerate case and stray whitespace from hand-written YAML.
	for k, v := range m {
		if strings.EqualFold(strings.TrimSpace(k), key) {
			return v, true
		}
	}
	return nil, false
}

func optString(m map[string]any, key string) (string, error) {
	v, ok := lookup(m, key)
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("openai runtime: %s must be a string, got %T", key, v)
	}
	return strings.TrimSpace(s), nil
}

func optBool(m map[string]any, key string) (bool, error) {
	v, ok := lookup(m, key)
	if !ok || v == nil {
		return false, nil
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			return false, fmt.Errorf("openai runtime: %s must be a boolean, got %q", key, t)
		}
		return b, nil
	default:
		return false, fmt.Errorf("openai runtime: %s must be a boolean, got %T", key, v)
	}
}

func optInt(m map[string]any, key string) (int64, bool, error) {
	v, ok := lookup(m, key)
	if !ok || v == nil {
		return 0, false, nil
	}
	switch t := v.(type) {
	case int:
		return int64(t), true, nil
	case int32:
		return int64(t), true, nil
	case int64:
		return t, true, nil
	case uint64:
		return int64(t), true, nil
	case float64:
		if t != float64(int64(t)) {
			return 0, false, fmt.Errorf("openai runtime: %s must be a whole number, got %v", key, t)
		}
		return int64(t), true, nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("openai runtime: %s must be a number, got %q", key, t)
		}
		return n, true, nil
	default:
		return 0, false, fmt.Errorf("openai runtime: %s must be a number, got %T", key, v)
	}
}

func optFloat(m map[string]any, key string) (float64, bool, error) {
	v, ok := lookup(m, key)
	if !ok || v == nil {
		return 0, false, nil
	}
	switch t := v.(type) {
	case float64:
		return t, true, nil
	case float32:
		return float64(t), true, nil
	case int:
		return float64(t), true, nil
	case int64:
		return float64(t), true, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false, fmt.Errorf("openai runtime: %s must be a number, got %q", key, t)
		}
		return f, true, nil
	default:
		return 0, false, fmt.Errorf("openai runtime: %s must be a number, got %T", key, v)
	}
}

// optDuration accepts a Go duration string ("90s", "5m") or a plain number of
// seconds, because operators write both.
func optDuration(m map[string]any, key string) (time.Duration, bool, error) {
	v, ok := lookup(m, key)
	if !ok || v == nil {
		return 0, false, nil
	}
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			// A bare number in a string is seconds.
			if n, nerr := strconv.ParseFloat(s, 64); nerr == nil {
				return time.Duration(n * float64(time.Second)), true, nil
			}
			return 0, false, fmt.Errorf("openai runtime: %s must be a duration like \"90s\" or a number of seconds, got %q", key, t)
		}
		return d, true, nil
	case int:
		return time.Duration(t) * time.Second, true, nil
	case int64:
		return time.Duration(t) * time.Second, true, nil
	case float64:
		return time.Duration(t * float64(time.Second)), true, nil
	default:
		return 0, false, fmt.Errorf("openai runtime: %s must be a duration string or number of seconds, got %T", key, v)
	}
}

func optHeaders(m map[string]any, key string) (map[string]string, error) {
	v, ok := lookup(m, key)
	if !ok || v == nil {
		return nil, nil
	}
	out := map[string]string{}
	switch t := v.(type) {
	case map[string]string:
		for k, val := range t {
			out[k] = val
		}
	case map[string]any:
		for k, val := range t {
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("openai runtime: %s[%s] must be a string, got %T", key, k, val)
			}
			out[k] = s
		}
	case map[any]any: // yaml.v3 into interface{} with non-string keys
		for k, val := range t {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("openai runtime: %s has a non-string header name %v", key, k)
			}
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("openai runtime: %s[%s] must be a string, got %T", key, ks, val)
			}
			out[ks] = s
		}
	default:
		return nil, fmt.Errorf("openai runtime: %s must be a map of header name to value, got %T", key, v)
	}
	for k := range out {
		if strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("openai runtime: %s contains an empty header name", key)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
