// Package clientconfig loads optional client-side defaults for the unwedge CLI
// and the MCP bridge, so users can prime the daemon address and TLS material
// once instead of passing -addr/-ca/-cert/-key on every invocation.
//
// Values resolve with precedence flag > environment > config file > built-in
// default. This package computes the environment/config/built-in layer as the
// default for each flag; the flag package then lets an explicit flag win.
package clientconfig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPort is applied to an address that names a host but no port.
const DefaultPort = "7777"

// SRV service/proto for the unwedge daemon: a device is reached by looking up
// _unwedge._tcp.<host>, so a controller can host several instances on distinct
// ports without clients needing to know them.
const (
	SRVService = "unwedge"
	SRVProto   = "tcp"
)

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
	NoSRV      *bool  `yaml:"no_srv"`      // disable SRV discovery; dial addr/default port
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
	NoSRV      bool
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
		NoSRV:      boolPref(os.Getenv("UNWEDGE_NO_SRV"), f.NoSRV, false),
		Path:       used,
	}, nil
}

// SRVFunc resolves DNS SRV records. It matches the subset of
// net.Resolver.LookupSRV used here and is injectable for tests.
type SRVFunc func(service, proto, name string) (cname string, addrs []*net.SRV, err error)

// ResolveEndpoint turns a user-facing daemon address into a concrete dial
// target (host:port) and, when necessary, the TLS server name to verify against.
//
// If addr already carries a port it is dialed verbatim. If addr names a host
// with no port and allowSRV is set, ResolveEndpoint looks up _unwedge._tcp.<host>
// and dials the record's target:port. When that target host differs from <host>
// it returns <host> as the server name, so TLS is still verified against the
// name the user asked for rather than the SRV target (which plain DNS does not
// authenticate; RFC 2782/6125). With no record — or when addr is an IP literal
// or already has a port — it falls back to appending DefaultPort and returns an
// empty server name (TLS then verifies against the dial host, as before).
//
// lookup is injectable for tests; pass nil to use the default resolver.
func ResolveEndpoint(addr string, allowSRV bool, lookup SRVFunc) (target, serverName string, err error) {
	if addr == "" {
		return "", "", fmt.Errorf("clientconfig: empty address")
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr, "", nil // explicit port: dial as-is
	}
	host := addr
	if !allowSRV || net.ParseIP(host) != nil {
		return EnsurePort(host, DefaultPort), "", nil
	}
	if lookup == nil {
		lookup = defaultLookupSRV
	}
	_, addrs, lerr := lookup(SRVService, SRVProto, host)
	if lerr != nil || len(addrs) == 0 {
		return EnsurePort(host, DefaultPort), "", nil
	}
	rec := addrs[0] // net.LookupSRV returns records sorted by priority then weight
	tgt := strings.TrimSuffix(rec.Target, ".")
	sni := ""
	if !strings.EqualFold(tgt, host) {
		sni = host
	}
	return net.JoinHostPort(tgt, strconv.Itoa(int(rec.Port))), sni, nil
}

func defaultLookupSRV(service, proto, name string) (string, []*net.SRV, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupSRV(ctx, service, proto, name)
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
