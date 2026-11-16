package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateCLIConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	unsetEnvForTest(t, "KILN_VM_API_URL")
	unsetEnvForTest(t, "KILN_VM_TOKEN")
	unsetEnvForTest(t, "KILN_VM_ALLOW_INSECURE_HTTP")
}

func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	previous, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, previous)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestLoadConfigRejectsMalformedJSON(t *testing.T) {
	isolateCLIConfig(t)

	dir := filepath.Dir(configPath())
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(configPath(), []byte(`{"api_url":`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected malformed config error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("expected parse config error, got %v", err)
	}
}

func TestLoadConfigAllowsEnvOnly(t *testing.T) {
	isolateCLIConfig(t)
	t.Setenv("KILN_VM_API_URL", "http://127.0.0.1:8080")
	t.Setenv("KILN_VM_TOKEN", "kiln_vm_test")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.APIURL != "http://127.0.0.1:8080" {
		t.Fatalf("APIURL = %q", cfg.APIURL)
	}
	if cfg.Token != "kiln_vm_test" {
		t.Fatalf("Token = %q", cfg.Token)
	}
}

func TestClientConfigRejectsRemotePlaintextWithoutExplicitOptIn(t *testing.T) {
	isolateCLIConfig(t)

	dir := filepath.Dir(configPath())
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(configPath(), []byte(`{"api_url":"http://192.0.2.10:8080","token":"kiln_vm_test"}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := NewClientFromConfig(); err == nil {
		t.Fatal("NewClientFromConfig accepted a remote plaintext API URL without explicit opt-in")
	}
}

func TestValidateAPIURLTransportPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		apiURL            string
		allowInsecureHTTP bool
		wantErr           bool
	}{
		{name: "remote HTTPS", apiURL: "https://controller.example:8443", wantErr: false},
		{name: "localhost HTTP", apiURL: "http://localhost:8080", wantErr: false},
		{name: "normalized localhost HTTP", apiURL: "http://LOCALHOST.:8080", wantErr: false},
		{name: "IPv4 loopback HTTP", apiURL: "http://127.42.0.9:8080", wantErr: false},
		{name: "IPv6 loopback HTTP", apiURL: "http://[::1]:8080", wantErr: false},
		{name: "remote HTTP rejected", apiURL: "http://192.0.2.10:8080", wantErr: true},
		{name: "remote HTTP explicitly allowed", apiURL: "http://192.0.2.10:8080", allowInsecureHTTP: true, wantErr: false},
		{name: "private DNS is not loopback", apiURL: "http://kiln-vm.internal:8080", wantErr: true},
		{name: "unsupported scheme", apiURL: "ssh://localhost:22", wantErr: true},
		{name: "relative URL", apiURL: "localhost:8080", wantErr: true},
		{name: "userinfo", apiURL: "https://user:password@controller.example", wantErr: true},
		{name: "fragment", apiURL: "https://controller.example/#api", wantErr: true},
		{name: "configured query", apiURL: "https://controller.example/?token=ambiguous", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAPIURL(test.apiURL, test.allowInsecureHTTP)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateAPIURL(%q, %t) error = %v, wantErr %t", test.apiURL, test.allowInsecureHTTP, err, test.wantErr)
			}
		})
	}
}

func TestValidateAPIURLDiagnosticsRedactSecretsAndNameRemediation(t *testing.T) {
	for _, apiURL := range []string{
		"https://user:sentinel-userinfo@controller.example",
		"https://controller.example/?token=sentinel-query",
		"https://controller.example/#sentinel-fragment",
		"https://controller.example/%sentinel-parse",
	} {
		err := validateAPIURL(apiURL, false)
		if err == nil {
			t.Fatalf("validateAPIURL(%q) succeeded", apiURL)
		}
		if strings.Contains(err.Error(), "sentinel") {
			t.Fatalf("validateAPIURL(%q) leaked secret in diagnostic %q", apiURL, err)
		}
	}

	err := validateAPIURL("http://controller.example:8080/path", false)
	if err == nil {
		t.Fatal("remote plaintext URL succeeded")
	}
	for _, want := range []string{"http://controller.example:8080", "kiln-vm login --allow-insecure-http", "KILN_VM_ALLOW_INSECURE_HTTP=true"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("remote plaintext diagnostic %q missing %q", err, want)
		}
	}
}

func TestLoadConfigEnvironmentURLDoesNotInheritPersistedConsent(t *testing.T) {
	isolateCLIConfig(t)
	if err := SaveConfig(&CLIConfig{
		APIURL:            "http://192.0.2.10:8080",
		Token:             "saved-token",
		AllowInsecureHTTP: true,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	t.Setenv("KILN_VM_API_URL", "http://198.51.100.20:8080")
	t.Setenv("KILN_VM_TOKEN", "environment-token")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.APIURL != "http://198.51.100.20:8080" || cfg.Token != "environment-token" {
		t.Fatalf("environment overrides not applied: %+v", cfg)
	}
	if cfg.AllowInsecureHTTP {
		t.Fatal("environment API URL inherited persisted insecure HTTP consent")
	}
	if _, err := NewClientFromConfig(); err == nil {
		t.Fatal("environment API URL was accepted without an environment opt-in")
	}

	t.Setenv("KILN_VM_ALLOW_INSECURE_HTTP", "true")
	client, err := NewClientFromConfig()
	if err != nil {
		t.Fatalf("NewClientFromConfig with environment opt-in: %v", err)
	}
	if !client.AllowInsecureHTTP {
		t.Fatal("environment opt-in was not applied to client")
	}

	saved, err := loadSavedConfig()
	if err != nil {
		t.Fatalf("loadSavedConfig: %v", err)
	}
	if saved.APIURL != "http://192.0.2.10:8080" || saved.Token != "saved-token" || !saved.AllowInsecureHTTP {
		t.Fatalf("environment overrides changed persisted config: %+v", saved)
	}
}

func TestLoadConfigRejectsInvalidInsecureHTTPEnvironmentValue(t *testing.T) {
	isolateCLIConfig(t)
	for _, value := range []string{"", "definitely", "TRUE", "1"} {
		t.Setenv("KILN_VM_ALLOW_INSECURE_HTTP", value)
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "KILN_VM_ALLOW_INSECURE_HTTP") {
			t.Fatalf("LoadConfig with %q error = %v, want invalid opt-in error", value, err)
		}
	}
}

func TestLoadConfigEnvironmentFalseOverridesPersistedConsent(t *testing.T) {
	isolateCLIConfig(t)
	if err := SaveConfig(&CLIConfig{
		APIURL:            "http://192.0.2.10:8080",
		Token:             "saved-token",
		AllowInsecureHTTP: true,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	t.Setenv("KILN_VM_ALLOW_INSECURE_HTTP", "false")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AllowInsecureHTTP {
		t.Fatal("environment false did not override persisted consent")
	}
	if _, err := NewClientFromConfig(); err == nil {
		t.Fatal("client accepted persisted remote HTTP after environment revoked consent")
	}
}

func TestLoadConfigExplicitEmptyEnvironmentOverridesPersistedValues(t *testing.T) {
	isolateCLIConfig(t)
	if err := SaveConfig(&CLIConfig{
		APIURL:            "http://192.0.2.10:8080",
		Token:             "saved-token",
		AllowInsecureHTTP: true,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	t.Setenv("KILN_VM_API_URL", "")
	t.Setenv("KILN_VM_TOKEN", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.APIURL != "" || cfg.Token != "" || cfg.AllowInsecureHTTP {
		t.Fatalf("empty environment did not override persisted values: %+v", cfg)
	}
}

func TestNewClientFromConfigRejectsExplicitEmptyEnvironmentCredentials(t *testing.T) {
	for _, tt := range []struct {
		name    string
		envKey  string
		wantErr string
	}{
		{name: "API URL", envKey: "KILN_VM_API_URL", wantErr: "API URL not configured"},
		{name: "token", envKey: "KILN_VM_TOKEN", wantErr: "token not configured"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateCLIConfig(t)
			if err := SaveConfig(&CLIConfig{APIURL: "http://127.0.0.1:8080", Token: "saved-token"}); err != nil {
				t.Fatalf("SaveConfig: %v", err)
			}
			t.Setenv(tt.envKey, "")

			client, err := NewClientFromConfig()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NewClientFromConfig error = %v, want containing %q", err, tt.wantErr)
			}
			if client != nil {
				t.Fatal("NewClientFromConfig returned a client for an explicitly empty environment value")
			}
		})
	}
}

func TestLoginRemoteHTTPRequiresAndPersistsExplicitFlag(t *testing.T) {
	isolateCLIConfig(t)

	withoutOptIn := newLoginCommand()
	withoutOptIn.SetOut(io.Discard)
	withoutOptIn.SetArgs([]string{"--api-url", "http://192.0.2.10:8080", "--token", "kiln_vm_test"})
	if err := withoutOptIn.Execute(); err == nil {
		t.Fatal("login accepted remote plaintext HTTP without --allow-insecure-http")
	}
	if _, err := os.Stat(configPath()); !os.IsNotExist(err) {
		t.Fatalf("rejected login wrote config, stat error = %v", err)
	}

	withOptIn := newLoginCommand()
	withOptIn.SetOut(io.Discard)
	withOptIn.SetArgs([]string{"--api-url", "http://192.0.2.10:8080", "--token", "kiln_vm_test", "--allow-insecure-http"})
	if err := withOptIn.Execute(); err != nil {
		t.Fatalf("login with explicit opt-in: %v", err)
	}
	saved, err := loadSavedConfig()
	if err != nil {
		t.Fatalf("loadSavedConfig: %v", err)
	}
	if !saved.AllowInsecureHTTP {
		t.Fatal("login did not persist explicit insecure HTTP opt-in")
	}
}

func TestLoginChangingAPIURLClearsStalePersistedOptIn(t *testing.T) {
	isolateCLIConfig(t)
	if err := SaveConfig(&CLIConfig{
		APIURL:            "http://192.0.2.10:8080",
		Token:             "saved-token",
		AllowInsecureHTTP: true,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	cmd := newLoginCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--api-url", "http://127.0.0.1:8080"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("login changing API URL: %v", err)
	}
	saved, err := loadSavedConfig()
	if err != nil {
		t.Fatalf("loadSavedConfig: %v", err)
	}
	if saved.APIURL != "http://127.0.0.1:8080" || saved.AllowInsecureHTTP {
		t.Fatalf("changed API URL retained stale opt-in: %+v", saved)
	}

	withNewConsent := newLoginCommand()
	withNewConsent.SetOut(io.Discard)
	withNewConsent.SetArgs([]string{"--api-url", "http://198.51.100.20:8080", "--allow-insecure-http"})
	if err := withNewConsent.Execute(); err != nil {
		t.Fatalf("login changing API URL with fresh opt-in: %v", err)
	}
	saved, err = loadSavedConfig()
	if err != nil {
		t.Fatalf("loadSavedConfig after fresh opt-in: %v", err)
	}
	if saved.APIURL != "http://198.51.100.20:8080" || !saved.AllowInsecureHTTP {
		t.Fatalf("changed API URL did not persist fresh opt-in: %+v", saved)
	}
}

func TestLoginMergesFileThenEnvironmentThenFlags(t *testing.T) {
	isolateCLIConfig(t)
	if err := SaveConfig(&CLIConfig{APIURL: "http://127.0.0.1:8080", Token: "saved-token"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	t.Setenv("KILN_VM_API_URL", "http://127.0.0.1:18080")
	t.Setenv("KILN_VM_TOKEN", "environment-token")

	cmd := newLoginCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--token", "flag-token"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("login: %v", err)
	}

	data, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var saved CLIConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if saved.APIURL != "http://127.0.0.1:18080" || saved.Token != "flag-token" || saved.AllowInsecureHTTP {
		t.Fatalf("login precedence result = %+v", saved)
	}
}

func TestLoginFinalSavedURLRetainsItsConsentAfterEnvironmentURLOverride(t *testing.T) {
	isolateCLIConfig(t)
	const savedURL = "http://192.0.2.10:8080"
	if err := SaveConfig(&CLIConfig{
		APIURL:            savedURL,
		Token:             "saved-token",
		AllowInsecureHTTP: true,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	t.Setenv("KILN_VM_API_URL", "http://198.51.100.20:8080")
	t.Setenv("KILN_VM_TOKEN", "environment-token")

	cmd := newLoginCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--api-url", savedURL, "--token", "flag-token"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("login: %v", err)
	}
	saved, err := loadSavedConfig()
	if err != nil {
		t.Fatalf("loadSavedConfig: %v", err)
	}
	if saved.APIURL != savedURL || saved.Token != "flag-token" || !saved.AllowInsecureHTTP {
		t.Fatalf("login final precedence result = %+v", saved)
	}
}

func TestLoginExplicitEmptyStringFlagsDoNotFallBackToLowerPrecedenceValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "API URL", args: []string{"--api-url", ""}},
		{name: "token", args: []string{"--token", ""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateCLIConfig(t)
			original := &CLIConfig{APIURL: "http://127.0.0.1:8080", Token: "saved-token"}
			if err := SaveConfig(original); err != nil {
				t.Fatalf("SaveConfig: %v", err)
			}
			t.Setenv("KILN_VM_API_URL", "http://127.0.0.1:18080")
			t.Setenv("KILN_VM_TOKEN", "environment-token")

			cmd := newLoginCommand()
			cmd.SetOut(io.Discard)
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "both --api-url and --token are required") {
				t.Fatalf("login error = %v, want required-value failure", err)
			}

			saved, err := loadSavedConfig()
			if err != nil {
				t.Fatalf("loadSavedConfig: %v", err)
			}
			if saved.APIURL != original.APIURL || saved.Token != original.Token || saved.AllowInsecureHTTP != original.AllowInsecureHTTP {
				t.Fatalf("rejected login changed persisted config: %+v", saved)
			}
		})
	}
}

func TestLoginExplicitEmptyEnvironmentDoesNotFallBackToSavedValues(t *testing.T) {
	for _, envKey := range []string{"KILN_VM_API_URL", "KILN_VM_TOKEN"} {
		t.Run(envKey, func(t *testing.T) {
			isolateCLIConfig(t)
			original := &CLIConfig{APIURL: "http://127.0.0.1:8080", Token: "saved-token"}
			if err := SaveConfig(original); err != nil {
				t.Fatalf("SaveConfig: %v", err)
			}
			t.Setenv(envKey, "")

			cmd := newLoginCommand()
			cmd.SetOut(io.Discard)
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "both --api-url and --token are required") {
				t.Fatalf("login error = %v, want required-value failure", err)
			}
			saved, err := loadSavedConfig()
			if err != nil {
				t.Fatalf("loadSavedConfig: %v", err)
			}
			if saved.APIURL != original.APIURL || saved.Token != original.Token {
				t.Fatalf("rejected login changed persisted config: %+v", saved)
			}
		})
	}
}

func TestLoginDoesNotPersistEnvironmentInsecureHTTPConsent(t *testing.T) {
	isolateCLIConfig(t)
	t.Setenv("KILN_VM_API_URL", "http://192.0.2.10:8080")
	t.Setenv("KILN_VM_TOKEN", "environment-token")
	t.Setenv("KILN_VM_ALLOW_INSECURE_HTTP", "true")

	cmd := newLoginCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "refusing plaintext HTTP") {
		t.Fatalf("login error = %v, want explicit persisted-consent rejection", err)
	}
	if _, err := os.Stat(configPath()); !os.IsNotExist(err) {
		t.Fatalf("rejected environment-only consent wrote config, stat error = %v", err)
	}
}

func TestLoginEnvironmentFalseRevokesPersistedInsecureHTTPConsent(t *testing.T) {
	isolateCLIConfig(t)
	if err := SaveConfig(&CLIConfig{
		APIURL:            "http://192.0.2.10:8080",
		Token:             "saved-token",
		AllowInsecureHTTP: true,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	t.Setenv("KILN_VM_ALLOW_INSECURE_HTTP", "false")

	cmd := newLoginCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--token", "replacement-token"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "refusing plaintext HTTP") {
		t.Fatalf("login error = %v, want environment revocation rejection", err)
	}

	saved, err := loadSavedConfig()
	if err != nil {
		t.Fatalf("loadSavedConfig: %v", err)
	}
	if saved.Token != "saved-token" || !saved.AllowInsecureHTTP {
		t.Fatalf("rejected login changed persisted config: %+v", saved)
	}
}

func TestLoginSurfacesInvalidEnvironmentConsent(t *testing.T) {
	isolateCLIConfig(t)
	if err := SaveConfig(&CLIConfig{APIURL: "http://127.0.0.1:8080", Token: "saved-token"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	t.Setenv("KILN_VM_ALLOW_INSECURE_HTTP", "TRUE")

	cmd := newLoginCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--token", "must-not-be-saved"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "must be exactly true or false") {
		t.Fatalf("login error = %v, want invalid environment error", err)
	}
	saved, err := loadSavedConfig()
	if err != nil {
		t.Fatalf("loadSavedConfig: %v", err)
	}
	if saved.Token != "saved-token" {
		t.Fatalf("failed login mutated saved token to %q", saved.Token)
	}
}
