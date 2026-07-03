package clientconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// clearEnv unsets every UNWEDGE_* var the package reads so a test starts clean.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"UNWEDGE_CONFIG", "UNWEDGE_ADDR", "UNWEDGE_CA", "UNWEDGE_CERT",
		"UNWEDGE_KEY", "UNWEDGE_SERVER_NAME", "UNWEDGE_NO_TLS", "UNWEDGE_INSECURE",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestResolveFileThenEnvPrecedence(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(
		"addr: file-host:9000\nca: /file/ca.crt\ncert: /file/cert.crt\nkey: /file/key.key\ninsecure: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// File-only.
	r, err := Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Addr != "file-host:9000" || r.CA != "/file/ca.crt" || !r.Insecure {
		t.Fatalf("file values not applied: %+v", r)
	}
	if r.Path != path {
		t.Fatalf("Path = %q, want %q", r.Path, path)
	}

	// Env overrides the file.
	t.Setenv("UNWEDGE_ADDR", "env-host:1")
	t.Setenv("UNWEDGE_CA", "/env/ca.crt")
	r, err = Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Addr != "env-host:1" || r.CA != "/env/ca.crt" {
		t.Fatalf("env did not override file: %+v", r)
	}
	if r.Cert != "/file/cert.crt" {
		t.Fatalf("unset env should fall through to file: %+v", r)
	}
}

func TestResolveBuiltinDefaultAddr(t *testing.T) {
	clearEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // point default path at an empty dir
	r, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if r.Addr != "localhost:7777" {
		t.Fatalf("Addr = %q, want localhost:7777", r.Addr)
	}
	if r.Path != "" {
		t.Fatalf("Path = %q, want empty (no file)", r.Path)
	}
}

func TestResolveMissingExplicitFileErrors(t *testing.T) {
	clearEnv(t)
	if _, err := Resolve(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for a named-but-missing config file")
	}
}

func TestEnsurePort(t *testing.T) {
	cases := map[string]string{
		"host":           "host:7777",
		"host:1234":      "host:1234",
		"":               "",
		"1.2.3.4":        "1.2.3.4:7777",
		"[::1]:22":       "[::1]:22",
		"controller.lab": "controller.lab:7777",
	}
	for in, want := range cases {
		if got := EnsurePort(in, "7777"); got != want {
			t.Errorf("EnsurePort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreScanConfig(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-config", "a.yaml", "status"}, "a.yaml"},
		{[]string{"--config", "b.yaml"}, "b.yaml"},
		{[]string{"-config=c.yaml", "status"}, "c.yaml"},
		{[]string{"--config=d.yaml"}, "d.yaml"},
		{[]string{"status"}, ""},
		{[]string{"--", "-config", "x"}, ""},
	}
	for _, c := range cases {
		if got := PreScanConfig(c.args); got != c.want {
			t.Errorf("PreScanConfig(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestExpandUser(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := expandUser("~/unwedge/ca.crt"); got != filepath.Join(home, "unwedge/ca.crt") {
		t.Errorf("expandUser(~/...) = %q", got)
	}
	if got := expandUser("/abs/path"); got != "/abs/path" {
		t.Errorf("absolute path changed: %q", got)
	}
}
