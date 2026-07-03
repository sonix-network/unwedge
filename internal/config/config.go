// Package config loads and validates the unwedged daemon configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level daemon configuration.
type Config struct {
	// Name identifies this instance when several unwedged processes share one
	// controller (one per device-under-test). It namespaces images in a shared
	// TFTP directory and labels logs. When unset it defaults to the config
	// filename ("dut1.yaml" -> "dut1"); the conventional single-instance file
	// "config.yaml" defaults to "" (no namespacing) for backward compatibility.
	Name    string        `yaml:"name"`
	Serial  SerialConfig  `yaml:"serial"`
	Power   PowerConfig   `yaml:"power"`
	UBoot   UBootConfig   `yaml:"uboot"`
	TFTP    TFTPConfig    `yaml:"tftp"`
	SSH     SSHConfig     `yaml:"ssh"`
	GRPC    GRPCConfig    `yaml:"grpc"`
	Session SessionConfig `yaml:"session"`
}

// SessionConfig configures the single-user hardware lock.
type SessionConfig struct {
	// Enabled turns on session locking. Default true.
	Enabled *bool `yaml:"enabled"`
	// TTL is the idle timeout after which an unrefreshed lock is released.
	TTL time.Duration `yaml:"ttl"`
}

// SerialConfig configures the console serial port.
type SerialConfig struct {
	// Device is the serial device path, e.g. the FTDI by-id symlink.
	Device string `yaml:"device"`
	// Baud rate; the vEdge 1000 console is 115200.
	Baud int `yaml:"baud"`
	// BufferBytes is the scrollback ring size. 0 uses a 1 MiB default.
	BufferBytes int `yaml:"buffer_bytes"`
}

// PowerConfig configures the APC PDU outlet control.
type PowerConfig struct {
	Address     string        `yaml:"address"`      // PDU host[:port]
	Community   string        `yaml:"community"`    // SNMP write community
	Outlet      int           `yaml:"outlet"`       // 1-based outlet number
	Version     string        `yaml:"version"`      // "v1" (default) or "v2c"
	OffDuration time.Duration `yaml:"off_duration"` // Cycle off time
	Timeout     time.Duration `yaml:"timeout"`
	Retries     int           `yaml:"retries"`
	// CommandOIDBase / StateOIDBase override APC defaults for other PDUs.
	CommandOIDBase string `yaml:"command_oid_base"`
	StateOIDBase   string `yaml:"state_oid_base"`
}

// UBootConfig configures the bootloader orchestration.
type UBootConfig struct {
	// ServerIP is the controller's IP address reachable from the target, used as
	// the TFTP serverip in U-Boot. Required for netboot.
	ServerIP         string        `yaml:"server_ip"`
	EthAct           string        `yaml:"ethact"`
	LoadAddr         string        `yaml:"load_addr"`
	CoreMask         string        `yaml:"coremask"`
	PromptPattern    string        `yaml:"prompt_pattern"`
	InterruptPattern string        `yaml:"interrupt_pattern"`
	InterruptKey     string        `yaml:"interrupt_key"`
	KernelBanner     string        `yaml:"kernel_banner"`
	CommandTimeout   time.Duration `yaml:"command_timeout"`
}

// TFTPConfig configures the image TFTP server.
type TFTPConfig struct {
	Dir     string `yaml:"dir"`     // image directory
	Address string `yaml:"address"` // UDP listen address, e.g. ":69"
	Enabled *bool  `yaml:"enabled"` // default true
}

// SSHConfig configures SSH access to the running target.
type SSHConfig struct {
	Host           string        `yaml:"host"`
	User           string        `yaml:"user"`
	Password       string        `yaml:"password"`
	PrivateKeyPath string        `yaml:"private_key_path"`
	KnownHostsPath string        `yaml:"known_hosts_path"`
	DialTimeout    time.Duration `yaml:"dial_timeout"`
}

// GRPCConfig configures the gRPC listener and TLS.
type GRPCConfig struct {
	Address string    `yaml:"address"` // e.g. ":7777"
	TLS     TLSConfig `yaml:"tls"`
}

// TLSConfig configures transport security. Because the service is intended to
// be exposed over the internet, TLS is strongly recommended; enable mutual TLS
// by setting ClientCAFile.
type TLSConfig struct {
	// Enabled turns on TLS. Default true; set explicitly to false only for a
	// trusted local socket.
	Enabled  *bool  `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ClientCAFile, if set, requires and verifies client certificates (mTLS).
	ClientCAFile string `yaml:"client_ca_file"`
}

// Default returns a Config populated with sensible vEdge 1000 defaults.
func Default() Config {
	trueVal := true
	return Config{
		Serial: SerialConfig{
			Device:      "/dev/serial/by-id/usb-FTDI_FT232R_USB_UART_ST163104-if00-port0",
			Baud:        115200,
			BufferBytes: 1 << 20,
		},
		Power: PowerConfig{
			Community:   "private",
			Outlet:      3,
			Version:     "v1",
			OffDuration: 5 * time.Second,
			Timeout:     3 * time.Second,
			Retries:     1,
		},
		UBoot: UBootConfig{
			EthAct:         "octmgmt0",
			LoadAddr:       "0x20000000",
			CoreMask:       "f",
			CommandTimeout: 30 * time.Second,
		},
		TFTP: TFTPConfig{
			Dir:     "/var/lib/unwedge/images",
			Address: ":69",
			Enabled: &trueVal,
		},
		SSH: SSHConfig{
			User:        "root",
			DialTimeout: 10 * time.Second,
		},
		GRPC: GRPCConfig{
			Address: ":7777",
			TLS:     TLSConfig{Enabled: &trueVal},
		},
		Session: SessionConfig{
			Enabled: &trueVal,
			TTL:     5 * time.Minute,
		},
	}
}

// SessionEnabled reports whether the hardware lock should be enforced.
func (c *Config) SessionEnabled() bool {
	return c.Session.Enabled == nil || *c.Session.Enabled
}

// Load reads YAML from path, overlaying it on Default, then validates.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	// Decode into the defaulted config so unspecified fields keep their defaults.
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if cfg.Name == "" {
		cfg.Name = defaultInstanceName(path)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// defaultInstanceName derives the instance name from the config filename, e.g.
// "/etc/unwedge/dut1.yaml" -> "dut1". The conventional single-instance file
// "config.yaml" yields "" so an upgraded single-instance controller keeps its
// existing (un-namespaced) on-disk image layout.
func defaultInstanceName(path string) string {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	if name == "config" {
		return ""
	}
	return name
}

// TFTPEnabled reports whether the TFTP server should run.
func (c *Config) TFTPEnabled() bool { return c.TFTP.Enabled == nil || *c.TFTP.Enabled }

// TLSEnabled reports whether TLS should be used.
func (c *Config) TLSEnabled() bool { return c.GRPC.TLS.Enabled == nil || *c.GRPC.TLS.Enabled }

// instanceNameRe restricts instance names to a filename-safe set so they can be
// used verbatim as an image-name prefix without escaping. "--" is disallowed
// because it is the prefix separator (see tftp.Store namespacing).
var instanceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Validate checks for internally inconsistent or missing required values.
func (c *Config) Validate() error {
	if c.Name != "" && (!instanceNameRe.MatchString(c.Name) || strings.Contains(c.Name, "--")) {
		return fmt.Errorf(`config: name %q is invalid (use letters, digits, '.', '_', '-'; no "--")`, c.Name)
	}
	if c.Serial.Device == "" {
		return fmt.Errorf("config: serial.device is required")
	}
	if c.Serial.Baud <= 0 {
		return fmt.Errorf("config: serial.baud must be positive")
	}
	if c.GRPC.Address == "" {
		return fmt.Errorf("config: grpc.address is required")
	}
	if c.TLSEnabled() {
		if c.GRPC.TLS.CertFile == "" || c.GRPC.TLS.KeyFile == "" {
			return fmt.Errorf("config: grpc.tls.cert_file and key_file are required when TLS is enabled")
		}
	}
	if c.TFTPEnabled() && c.TFTP.Dir == "" {
		return fmt.Errorf("config: tftp.dir is required when TFTP is enabled")
	}
	// Power is optional (a controller may lack PDU access); validate only if set.
	if c.Power.Address != "" && c.Power.Outlet < 1 {
		return fmt.Errorf("config: power.outlet must be >= 1 when power.address is set")
	}
	return nil
}
