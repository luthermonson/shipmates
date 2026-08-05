package openai

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfig_MinimalAppliesDefaults(t *testing.T) {
	cfg, err := ParseConfig(map[string]any{
		"base_url": "https://inference.internal/v1",
		"model":    "moonshotai/Kimi-K2-Instruct",
	})
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.BaseURL != "https://inference.internal/v1" || cfg.Model != "moonshotai/Kimi-K2-Instruct" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", cfg.Timeout, DefaultTimeout)
	}
	if cfg.MaxResponseBytes != DefaultMaxResponseBytes || cfg.MaxLineBytes != DefaultMaxLineBytes {
		t.Errorf("byte bounds not defaulted: %+v", cfg)
	}
	if cfg.APIKeyEnv != "" {
		t.Errorf("APIKeyEnv should be empty (no-auth endpoints are supported), got %q", cfg.APIKeyEnv)
	}
	if cfg.Endpoint() != "https://inference.internal/v1/chat/completions" {
		t.Errorf("Endpoint() = %q", cfg.Endpoint())
	}
	if cfg.ModelsEndpoint() != "https://inference.internal/v1/models" {
		t.Errorf("ModelsEndpoint() = %q", cfg.ModelsEndpoint())
	}
}

func TestParseConfig_RequiredKeys(t *testing.T) {
	if _, err := ParseConfig(map[string]any{"model": "m"}); err == nil || !strings.Contains(err.Error(), "base_url is required") {
		t.Errorf("missing base_url error = %v", err)
	}
	if _, err := ParseConfig(map[string]any{"base_url": "https://x/v1"}); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("missing model error = %v", err)
	}
}

// A key pasted into config is a mistake we refuse loudly instead of honouring:
// config files get committed, shared, and pasted into issues.
func TestParseConfig_RejectsInlineSecrets(t *testing.T) {
	for _, key := range []string{"api_key", "apikey", "api-key", "token", "secret", "key"} {
		_, err := ParseConfig(map[string]any{
			"base_url": "https://inference.internal/v1",
			"model":    "m",
			key:        "sk-should-never-be-here",
		})
		if err == nil {
			t.Fatalf("setting %q was accepted", key)
		}
		if !strings.Contains(err.Error(), "api_key_env") {
			t.Errorf("error for %q should point at api_key_env, got: %v", key, err)
		}
		if strings.Contains(err.Error(), "sk-should-never-be-here") {
			t.Errorf("error for %q echoed the secret value: %v", key, err)
		}
	}
}

// There is no way to turn off certificate verification, and asking for one is
// an error rather than a no-op — an enterprise endpoint is exactly where a
// stray insecure flag would get shipped.
func TestParseConfig_RejectsTLSWeakening(t *testing.T) {
	for _, key := range []string{"insecure_skip_verify", "insecure", "skip_tls_verify", "tls_insecure", "verify_ssl"} {
		_, err := ParseConfig(map[string]any{
			"base_url": "https://inference.internal/v1",
			"model":    "m",
			key:        true,
		})
		if err == nil {
			t.Fatalf("setting %q was accepted", key)
		}
		if !strings.Contains(err.Error(), "ca_file") {
			t.Errorf("error for %q should point at ca_file, got: %v", key, err)
		}
	}
}

func TestParseConfig_UnknownKeyIsAnError(t *testing.T) {
	_, err := ParseConfig(map[string]any{
		"base_url": "https://inference.internal/v1",
		"model":    "m",
		"base_ur1": "typo",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("expected unknown-setting error, got %v", err)
	}
	// Factory metadata is tolerated so a wrapper can pass its block through.
	if _, err := ParseConfig(map[string]any{
		"base_url": "https://inference.internal/v1",
		"model":    "m",
		"runtime":  "openai",
		"name":     "openai",
	}); err != nil {
		t.Errorf("metadata keys rejected: %v", err)
	}
}

func TestParseConfig_TimeoutForms(t *testing.T) {
	cases := map[string]struct {
		val  any
		want time.Duration
	}{
		"duration string": {"90s", 90 * time.Second},
		"minutes string":  {"5m", 5 * time.Minute},
		"int seconds":     {45, 45 * time.Second},
		"float seconds":   {1.5, 1500 * time.Millisecond},
		"numeric string":  {"30", 30 * time.Second},
	}
	for name, tc := range cases {
		cfg, err := ParseConfig(map[string]any{
			"base_url": "https://x/v1", "model": "m", "timeout": tc.val,
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.Timeout != tc.want {
			t.Errorf("%s: Timeout = %v, want %v", name, cfg.Timeout, tc.want)
		}
	}
	if _, err := ParseConfig(map[string]any{"base_url": "https://x/v1", "model": "m", "timeout": "soon"}); err == nil {
		t.Errorf("garbage timeout accepted")
	}
}

func TestParseConfig_HeadersRejectAuthorization(t *testing.T) {
	_, err := ParseConfig(map[string]any{
		"base_url": "https://x/v1", "model": "m",
		"headers": map[string]any{"AuThOrIzAtIoN": "Bearer sk-nope"},
	})
	if err == nil || !strings.Contains(err.Error(), "api_key_env") {
		t.Fatalf("Authorization header override should be refused, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "sk-nope") {
		t.Errorf("error echoed the header value: %v", err)
	}
	// Normal extra headers are fine, including yaml's map[any]any shape.
	cfg, err := ParseConfig(map[string]any{
		"base_url": "https://x/v1", "model": "m",
		"headers": map[any]any{"X-Tenant": "gpu-cloud", "OpenAI-Project": "proj_1"},
	})
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	if cfg.Headers["X-Tenant"] != "gpu-cloud" || cfg.Headers["OpenAI-Project"] != "proj_1" {
		t.Errorf("headers not parsed: %+v", cfg.Headers)
	}
}

func TestParseConfig_PlaintextHTTPRules(t *testing.T) {
	// Loopback plaintext is the normal self-hosted case: allowed.
	if _, err := ParseConfig(map[string]any{"base_url": "http://127.0.0.1:11434/v1", "model": "m"}); err != nil {
		t.Errorf("loopback http rejected: %v", err)
	}
	if _, err := ParseConfig(map[string]any{"base_url": "http://localhost:8000/v1", "model": "m"}); err != nil {
		t.Errorf("localhost http rejected: %v", err)
	}
	// Remote plaintext needs an explicit opt-in.
	_, err := ParseConfig(map[string]any{"base_url": "http://inference.internal/v1", "model": "m"})
	if err == nil || !strings.Contains(err.Error(), "allow_plaintext_http") {
		t.Errorf("remote http should require opt-in, got %v", err)
	}
	if _, err := ParseConfig(map[string]any{
		"base_url": "http://inference.internal/v1", "model": "m", "allow_plaintext_http": true,
	}); err != nil {
		t.Errorf("opted-in remote http rejected: %v", err)
	}
	// A credential never goes over cleartext, opt-in or not.
	_, err = ParseConfig(map[string]any{
		"base_url": "http://inference.internal/v1", "model": "m",
		"api_key_env": "TOK", "allow_plaintext_http": true,
	})
	if err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("key over plaintext http should be refused even with opt-in, got %v", err)
	}
	// Nonsense schemes and embedded credentials are out.
	if _, err := ParseConfig(map[string]any{"base_url": "ftp://x/v1", "model": "m"}); err == nil {
		t.Errorf("ftp scheme accepted")
	}
	if _, err := ParseConfig(map[string]any{"base_url": "https://user:pw@x/v1", "model": "m"}); err == nil {
		t.Errorf("embedded credentials accepted")
	}
}

func TestConfig_EndpointVariants(t *testing.T) {
	cases := map[string]string{
		"https://x/v1":                   "https://x/v1/chat/completions",
		"https://x/v1/":                  "https://x/v1/chat/completions",
		"https://x/openai/v1///":         "https://x/openai/v1/chat/completions",
		"https://x/v1/chat/completions":  "https://x/v1/chat/completions",
		"https://x/v1/chat/completions/": "https://x/v1/chat/completions",
		" https://x/v1 ":                 "https://x/v1/chat/completions",
	}
	for base, want := range cases {
		cfg := Config{BaseURL: base, Model: "m"}
		if got := cfg.Endpoint(); got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestConfig_APIKeyFromEnv(t *testing.T) {
	cfg := Config{BaseURL: "https://x/v1", Model: "m"}

	// No api_key_env: no-auth endpoint, no error.
	key, err := cfg.apiKey()
	if err != nil || key != "" {
		t.Fatalf("no-auth: key=%q err=%v", key, err)
	}

	// Configured but unset: a distinct, actionable error that names the
	// variable and cannot contain a value (there isn't one).
	cfg.APIKeyEnv = "SHIPMATES_TEST_MISSING_KEY"
	t.Setenv("SHIPMATES_TEST_MISSING_KEY", "")
	if _, err := cfg.apiKey(); err == nil || !strings.Contains(err.Error(), "SHIPMATES_TEST_MISSING_KEY") {
		t.Fatalf("empty env should error naming the var, got %v", err)
	}

	// Whitespace-only counts as empty; real values are trimmed.
	t.Setenv("SHIPMATES_TEST_MISSING_KEY", "   \n")
	if _, err := cfg.apiKey(); err == nil {
		t.Errorf("whitespace-only key accepted")
	}
	t.Setenv("SHIPMATES_TEST_MISSING_KEY", "  sk-real-value  ")
	key, err = cfg.apiKey()
	if err != nil || key != "sk-real-value" {
		t.Fatalf("key=%q err=%v", key, err)
	}
}

func TestConfig_HTTPClientNeverWeakensTLS(t *testing.T) {
	cfg := Config{BaseURL: "https://x/v1", Model: "m"}
	hc, err := cfg.httpClient()
	if err != nil {
		t.Fatalf("httpClient: %v", err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T", hc.Transport)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil; MinVersion pinning was lost")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify is true")
	}
	if tr.TLSClientConfig.MinVersion < 0x0303 { // tls.VersionTLS12
		t.Errorf("MinVersion = %#x, want >= TLS 1.2", tr.TLSClientConfig.MinVersion)
	}
	if hc.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 (a streamed turn is bounded by context, not by a client deadline)", hc.Timeout)
	}
}

// The strongest guard available: no non-test source file in this package may
// even mention the field that disables verification.
func TestSourceContainsNoVerificationBypass(t *testing.T) {
	needle := "Insecure" + "SkipVerify" // split so this test is not its own match
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), needle) {
			t.Errorf("%s mentions %s", f, needle)
		}
	}
}

func TestConfig_CAFile(t *testing.T) {
	dir := t.TempDir()

	// Garbage is rejected at parse time, not at first request.
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseConfig(map[string]any{"base_url": "https://x/v1", "model": "m", "ca_file": bad}); err == nil {
		t.Errorf("non-PEM ca_file accepted")
	}
	if _, err := ParseConfig(map[string]any{"base_url": "https://x/v1", "model": "m", "ca_file": filepath.Join(dir, "nope.pem")}); err == nil {
		t.Errorf("missing ca_file accepted")
	}

	// A real self-signed CA is added to the pool, additively — the system
	// roots are the starting point, so a private CA does not break public
	// endpoints.
	good := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(good, selfSignedCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(map[string]any{"base_url": "https://x/v1", "model": "m", "ca_file": good})
	if err != nil {
		t.Fatalf("valid ca_file rejected: %v", err)
	}
	hc, err := cfg.httpClient()
	if err != nil {
		t.Fatalf("httpClient: %v", err)
	}
	tr := hc.Transport.(*http.Transport)
	if tr.TLSClientConfig.RootCAs == nil {
		t.Error("RootCAs not set from ca_file")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("ca_file path turned off verification")
	}
}

func TestParseConfig_BoundsMustBePositive(t *testing.T) {
	for _, key := range []string{"max_response_bytes", "max_prompt_bytes", "max_transcript_messages"} {
		if _, err := ParseConfig(map[string]any{"base_url": "https://x/v1", "model": "m", key: 0}); err == nil {
			t.Errorf("%s: zero accepted (zero must mean 'default', not 'unbounded')", key)
		}
		if _, err := ParseConfig(map[string]any{"base_url": "https://x/v1", "model": "m", key: -5}); err == nil {
			t.Errorf("%s: negative accepted", key)
		}
	}
	if _, err := ParseConfig(map[string]any{"base_url": "https://x/v1", "model": "m", "max_line_bytes": 16}); err == nil {
		t.Errorf("absurdly small max_line_bytes accepted")
	}
}

func TestParseConfig_OptionalModelParams(t *testing.T) {
	cfg, err := ParseConfig(map[string]any{
		"base_url": "https://x/v1", "model": "m",
		"temperature": 0.2, "max_tokens": 1024, "organization": "org-42",
	})
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.2 {
		t.Errorf("Temperature = %v", cfg.Temperature)
	}
	if cfg.MaxTokens != 1024 || cfg.Organization != "org-42" {
		t.Errorf("unexpected: %+v", cfg)
	}
	// Unset temperature stays out of the request body entirely.
	cfg, err = ParseConfig(map[string]any{"base_url": "https://x/v1", "model": "m"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Temperature != nil {
		t.Errorf("Temperature should be nil when unset, got %v", *cfg.Temperature)
	}
}

// selfSignedCAPEM mints a throwaway CA certificate so the ca_file path can be
// exercised without shipping a fixture.
func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "shipmates-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
