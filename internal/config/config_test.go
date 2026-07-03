package config

import (
	"os"
	"path/filepath"
	"testing"
)

// minimalConfig is the least YAML that passes Validate (serial + TLS disabled).
const minimalConfig = `
serial:
  device: /dev/ttyUSB0
grpc:
  tls:
    enabled: false
`

func writeConfig(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestInstanceNameDefaultsFromFilename(t *testing.T) {
	cfg, err := Load(writeConfig(t, "dut1.yaml", minimalConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "dut1" {
		t.Fatalf("name = %q, want dut1", cfg.Name)
	}
}

func TestConfigYamlDefaultsToEmptyName(t *testing.T) {
	// The conventional single-instance file gets no namespace, so an upgraded
	// controller keeps its existing on-disk image layout.
	cfg, err := Load(writeConfig(t, "config.yaml", minimalConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "" {
		t.Fatalf("name = %q, want empty for config.yaml", cfg.Name)
	}
}

func TestExplicitNameOverridesFilename(t *testing.T) {
	cfg, err := Load(writeConfig(t, "dut1.yaml", minimalConfig+"name: edge-b\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "edge-b" {
		t.Fatalf("name = %q, want edge-b", cfg.Name)
	}
}

func TestInvalidNameRejected(t *testing.T) {
	for _, bad := range []string{"has space", "a/b", "with--dashes", "-leading"} {
		_, err := Load(writeConfig(t, "x.yaml", minimalConfig+"name: '"+bad+"'\n"))
		if err == nil {
			t.Errorf("expected rejection of name %q", bad)
		}
	}
}
