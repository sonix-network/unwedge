// Package clientconfig loads optional client-side defaults for the unwedge CLI
// and the MCP bridge, so users can prime the daemon address and TLS material
// once instead of passing -addr/-ca/-cert/-key on every invocation.
//
// Values resolve with precedence flag > environment > config file > built-in
// default. This package computes the environment/config/built-in layer as the
// default for each flag; the flag package then lets an explicit flag win.
package clientconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPort is applied to an address that names a host but no port.
const DefaultPort = "7777"

// File is the on-disk client config. Every field is optional; a missing file is
// not an error unless it was named explicitly (via -config or UNWEDGE_CONFIG).
// Paths may start with ~ or $HOME and are expanded on load.
type File struct {
	Addr       string `yaml:"addr"`        // daemon host:port (port optional; defaults to 7777)
	CA         string `yaml:"ca"`          // CA cert verifying the server
	Cert       string `yaml:"cert"`        // client cert (mTLS)
	Key        string `yaml:"key"`         // client key (mTLS)
	ServerName string `yaml:"server_name"` // override TLS server name
	NoTLS      *bool  `yaml:"no_tls"`      // connect without TLS (local/testing)
	Insecure   *bool  `yaml:"insecure"`    // skip server cert verification (dev)
}

// Resolved holds the effective flag defaults after merging the config file with
// the environment. Callers seed their flags with these; a flag set on the
// command line then overrides. Path is the config file actually loaded (empty
// if none was found).
type Resolved struct {
	Addr       string
	CA         string
	Cert       string
	Key        string
	ServerName string
	NoTLS      bool
	Insecure   bool
	Path       string
}

// DefaultPath returns the config file location searched when neither -config nor
// UNWEDGE_CONFIG is set: $XDG_CONFIG_HOME/unwedge/config.yaml, falling back to
// ~/.config/unwedge/config.yaml. It returns "" if the home directory is unknown.
func DefaultPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "unwedge", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "unwedge", "config.yaml")
}

// Resolve loads the config file (if any) and merges it with the environment to
// produce flag defaults. configFlag is the value of a -config flag if one was
// supplied (see PreScanConfig); when empty, UNWEDGE_CONFIG then DefaultPath are
// tried. A named-but-missing file is an error; a missing default file is not.
func Resolve(configFlag string) (Resolved, error) {
	path, explicit := configFlag, configFlag != ""
	if path == "" {
		if env := os.Getenv("UNWEDGE_CONFIG"); env != "" {
			path, explicit = env, true
		} else {
			path = DefaultPath()
		}
	}

	var f File
	var used string
	if path != "" {
		b, err := os.ReadFile(expandUser(path))
		switch {
		case err == nil:
			if err := yaml.Unmarshal(b, &f); err != nil {
				return Resolved{}, fmt.Errorf("clientconfig: parse %s: %w", path, err)
			}
			used = path
		case explicit || !errors.Is(err, fs.ErrNotExist):
			return Resolved{}, fmt.Errorf("clientconfig: read %s: %w", path, err)
		}
	}

	return Resolved{
		Addr:       firstNonEmpty(os.Getenv("UNWEDGE_ADDR"), f.Addr, "localhost:7777"),
		CA:         expandUser(firstNonEmpty(os.Getenv("UNWEDGE_CA"), f.CA)),
		Cert:       expandUser(firstNonEmpty(os.Getenv("UNWEDGE_CERT"), f.Cert)),
		Key:        expandUser(firstNonEmpty(os.Getenv("UNWEDGE_KEY"), f.Key)),
		ServerName: firstNonEmpty(os.Getenv("UNWEDGE_SERVER_NAME"), f.ServerName),
		NoTLS:      boolPref(os.Getenv("UNWEDGE_NO_TLS"), f.NoTLS, false),
		Insecure:   boolPref(os.Getenv("UNWEDGE_INSECURE"), f.Insecure, false),
		Path:       used,
	}, nil
}

// EnsurePort appends ":port" to addr when it names a host but no port, so a
// config or flag value like "controller.example" reaches the default port.
func EnsurePort(addr, port string) string {
	if addr == "" {
		return addr
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr // already has a port
	}
	return net.JoinHostPort(addr, port)
}

// PreScanConfig extracts the value of a -config/--config flag from args before
// the main flag set is parsed, so the config file can be located in time to seed
// the other flags' defaults. Supports "-config X", "-config=X" and the "--"
// forms. It returns "" if the flag is absent.
func PreScanConfig(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		name, val, hasEq := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name != "config" {
			continue
		}
		if hasEq {
			return val
		}
		if i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// expandUser expands a leading ~ or $HOME in a path so config files can use the
// natural "~/unwedge/ca.crt" form. Non-path or already-absolute values pass
// through unchanged.
func expandUser(p string) string {
	if p == "" {
		return p
	}
	switch {
	case p == "~":
		p = "$HOME"
	case strings.HasPrefix(p, "~/"):
		p = "$HOME" + p[1:]
	}
	if strings.HasPrefix(p, "$HOME") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home + p[len("$HOME"):]
		}
	}
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// boolPref resolves a boolean with precedence env > file > def. An unparseable
// or empty env value is ignored so it falls through to the file/default.
func boolPref(env string, file *bool, def bool) bool {
	if env != "" {
		if b, err := strconv.ParseBool(env); err == nil {
			return b
		}
	}
	if file != nil {
		return *file
	}
	return def
}
