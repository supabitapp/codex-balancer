package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

type configuredCodex struct {
	ModelProvider string `toml:"model_provider"`
	Analytics     struct {
		Enabled bool `toml:"enabled"`
	} `toml:"analytics"`
	Feedback struct {
		Enabled bool `toml:"enabled"`
	} `toml:"feedback"`
	ModelProviders map[string]struct {
		Name               string            `toml:"name"`
		BaseURL            string            `toml:"base_url"`
		EnvKey             string            `toml:"env_key"`
		RequiresOpenAIAuth bool              `toml:"requires_openai_auth"`
		SupportsWebSockets bool              `toml:"supports_websockets"`
		HTTPHeaders        map[string]string `toml:"http_headers"`
	} `toml:"model_providers"`
}

func TestConfigureCodexDocumentCreatesConfig(t *testing.T) {
	configured, err := configureCodexDocument(nil, "https://balancer.example/v1")
	if err != nil {
		t.Fatal(err)
	}
	want := `model_provider = "balancer"

[analytics]
enabled = false

[feedback]
enabled = false

[model_providers.balancer]
name = "OpenAI"
base_url = "https://balancer.example/v1"
env_key = "CODEX_BALANCER_API_KEY"
requires_openai_auth = true
supports_websockets = true
`
	if string(configured) != want {
		t.Fatalf("configured document =\n%s\nwant:\n%s", configured, want)
	}
}

func TestConfigureCodexDocumentUpdatesOnlyBalancerSettings(t *testing.T) {
	existing := `# retained
model = "gpt-5.6-sol"
model_provider = "old" # retained inline

[analytics]
enabled = true # retained analytics

[feedback]
enabled = true

[model_providers.balancer]
name = "Old"
base_url = "https://old.example/v1"
env_key = "OLD_KEY"
requires_openai_auth = false
supports_websockets = false

[features]
shell_snapshot = true
`
	configured, err := configureCodexDocument([]byte(existing), "https://new.example/v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, retained := range []string{"# retained\n", "# retained inline", "# retained analytics", "model = \"gpt-5.6-sol\"", "shell_snapshot = true"} {
		if !bytes.Contains(configured, []byte(retained)) {
			t.Fatalf("configured document dropped %q:\n%s", retained, configured)
		}
	}
	assertConfiguredCodex(t, configured, "https://new.example/v1")
	again, err := configureCodexDocument(configured, "https://new.example/v1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configured, again) {
		t.Fatalf("second configuration changed document:\n%s", again)
	}
}

func TestConfigureCodexDocumentExtendsDottedProvider(t *testing.T) {
	existing := `model_providers.balancer.name = "Old"

[model_providers.balancer.http_headers]
x-client = "retained"
`
	configured, err := configureCodexDocument([]byte(existing), "http://127.0.0.1:8317/v1")
	if err != nil {
		t.Fatal(err)
	}
	config := assertConfiguredCodex(t, configured, "http://127.0.0.1:8317/v1")
	if config.ModelProviders["balancer"].HTTPHeaders["x-client"] != "retained" {
		t.Fatalf("HTTP headers = %v", config.ModelProviders["balancer"].HTTPHeaders)
	}
}

func TestConfigureCodexDocumentCompletesExistingSections(t *testing.T) {
	existing := `[analytics]

[feedback]

[model_providers.balancer]
http_headers = { x-client = "retained" }
`
	configured, err := configureCodexDocument([]byte(existing), "http://127.0.0.1:8317/v1")
	if err != nil {
		t.Fatal(err)
	}
	config := assertConfiguredCodex(t, configured, "http://127.0.0.1:8317/v1")
	if config.ModelProviders["balancer"].HTTPHeaders["x-client"] != "retained" {
		t.Fatalf("HTTP headers = %v", config.ModelProviders["balancer"].HTTPHeaders)
	}
}

func TestConfigureCodexWritesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex", "config.toml")
	if err := configureCodex(path, "http://127.0.0.1:8317/v1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertConfiguredCodex(t, data, "http://127.0.0.1:8317/v1")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestConfigureCodexRejectsInvalidConfigWithoutChangingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := []byte("[broken\n")
	if err := os.WriteFile(path, existing, 0o640); err != nil {
		t.Fatal(err)
	}
	err := configureCodex(path, "http://127.0.0.1:8317/v1")
	if err == nil || !strings.Contains(err.Error(), "invalid existing config") {
		t.Fatalf("configure error = %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(data, existing) {
		t.Fatalf("invalid config changed to %q", data)
	}
}

func TestConfigureCodexRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.toml")
	path := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := configureCodex(path, "http://127.0.0.1:8317/v1"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("configure error = %v", err)
	}
}

func TestValidateProviderURL(t *testing.T) {
	got, err := validateProviderURL("https://balancer.example/v1/")
	if err != nil || got != "https://balancer.example/v1" {
		t.Fatalf("valid URL = %q, %v", got, err)
	}
	for _, value := range []string{"", "ftp://balancer.example/v1", "https://user@balancer.example/v1", "https://balancer.example/v1?q=1", "https://balancer.example/v1#fragment"} {
		if _, err := validateProviderURL(value); err == nil {
			t.Fatalf("accepted invalid URL %q", value)
		}
	}
}

func assertConfiguredCodex(t *testing.T, data []byte, baseURL string) configuredCodex {
	t.Helper()
	var config configuredCodex
	if err := toml.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse configured document: %v\n%s", err, data)
	}
	provider := config.ModelProviders["balancer"]
	if config.ModelProvider != "balancer" || config.Analytics.Enabled || config.Feedback.Enabled || provider.Name != "OpenAI" || provider.BaseURL != baseURL || provider.EnvKey != "CODEX_BALANCER_API_KEY" || !provider.RequiresOpenAIAuth || !provider.SupportsWebSockets {
		t.Fatalf("configured values = %+v", config)
	}
	return config
}
