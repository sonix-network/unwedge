package power

import (
	"context"
	"fmt"
	"time"

	"github.com/gosnmp/gosnmp"
)

// APC PowerNet-MIB OIDs for the classic Switched Rack PDU (rPDU) family
// (AP79xx / AP89xx). These are the broadly-compatible pre-rPDU2 OIDs.
//
//	rPDUOutletControlOutletCommand (RW): set to command the outlet.
//	rPDUOutletStatusOutletState    (RO): read to learn the outlet state.
//
// The trailing ".N" selects outlet N.
const (
	oidOutletCommand = "1.3.6.1.4.1.318.1.1.12.3.3.1.1.4"
	oidOutletState   = "1.3.6.1.4.1.318.1.1.12.3.5.1.1.4"
)

// rPDUOutletControlOutletCommand values.
const (
	cmdImmediateOn     = 1
	cmdImmediateOff    = 2
	cmdImmediateReboot = 3
)

// rPDUOutletStatusOutletState values.
const (
	stateOn  = 1
	stateOff = 2
)

// APCConfig configures an APC PDU controller.
type APCConfig struct {
	// Address is the PDU host or host:port. Port defaults to 161.
	Address string
	// Community is the SNMP community. Writing outlet commands requires the
	// write community (often "private").
	Community string
	// Outlet is the 1-based outlet number (3 in the reference setup).
	Outlet int
	// Version selects SNMP v1 or v2c. Empty defaults to v1, which the classic
	// rPDU firmware always supports.
	Version string
	// Timeout for each SNMP request. 0 -> 3s.
	Timeout time.Duration
	// Retries per request. Applied as-is.
	Retries int
	// CommandOIDBase / StateOIDBase override the OID bases (without the outlet
	// suffix) for non-standard PDUs. Empty uses the APC defaults above.
	CommandOIDBase string
	StateOIDBase   string
	// OffDuration overrides Cycle's default off time. 0 uses DefaultOffDuration.
	OffDuration time.Duration
}

// APC is a Controller backed by an APC PDU over SNMP.
type APC struct {
	cfg    APCConfig
	cmdOID string
	stOID  string
	dialer func() (snmpConn, error)
}

// snmpConn is the subset of gosnmp used here; abstracted for testability.
type snmpConn interface {
	Connect() error
	Get(oids []string) (*gosnmp.SnmpPacket, error)
	Set(pdus []gosnmp.SnmpPDU) (*gosnmp.SnmpPacket, error)
	Close() error
}

// NewAPC builds an APC controller from config.
func NewAPC(cfg APCConfig) (*APC, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("power: APC address required")
	}
	if cfg.Outlet < 1 {
		return nil, fmt.Errorf("power: APC outlet must be >= 1, got %d", cfg.Outlet)
	}
	cmdBase := cfg.CommandOIDBase
	if cmdBase == "" {
		cmdBase = oidOutletCommand
	}
	stBase := cfg.StateOIDBase
	if stBase == "" {
		stBase = oidOutletState
	}
	a := &APC{
		cfg:    cfg,
		cmdOID: fmt.Sprintf("%s.%d", cmdBase, cfg.Outlet),
		stOID:  fmt.Sprintf("%s.%d", stBase, cfg.Outlet),
	}
	a.dialer = a.defaultDial
	return a, nil
}

func (a *APC) defaultDial() (snmpConn, error) {
	host, port := splitHostPort(a.cfg.Address, 161)
	timeout := a.cfg.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	community := a.cfg.Community
	if community == "" {
		community = "private"
	}
	version := gosnmp.Version1
	if a.cfg.Version == "2c" || a.cfg.Version == "v2c" || a.cfg.Version == "2" {
		version = gosnmp.Version2c
	}
	g := &gosnmp.GoSNMP{
		Target:    host,
		Port:      port,
		Community: community,
		Version:   version,
		Timeout:   timeout,
		Retries:   a.cfg.Retries,
	}
	return g, nil
}

func (a *APC) do(fn func(snmpConn) error) error {
	conn, err := a.dialer()
	if err != nil {
		return err
	}
	if err := conn.Connect(); err != nil {
		return fmt.Errorf("power: snmp connect %s: %w", a.cfg.Address, err)
	}
	defer conn.Close()
	return fn(conn)
}

func (a *APC) setCommand(cmd int) error {
	return a.do(func(conn snmpConn) error {
		pdu := gosnmp.SnmpPDU{Name: a.cmdOID, Type: gosnmp.Integer, Value: cmd}
		resp, err := conn.Set([]gosnmp.SnmpPDU{pdu})
		if err != nil {
			return fmt.Errorf("power: snmp set %s=%d: %w", a.cmdOID, cmd, err)
		}
		if resp != nil && resp.Error != gosnmp.NoError {
			return fmt.Errorf("power: snmp set %s returned %v", a.cmdOID, resp.Error)
		}
		return nil
	})
}

// On implements Controller.
func (a *APC) On(ctx context.Context) error { return a.setCommand(cmdImmediateOn) }

// Off implements Controller.
func (a *APC) Off(ctx context.Context) error { return a.setCommand(cmdImmediateOff) }

// Status implements Controller.
func (a *APC) Status(ctx context.Context) (State, error) {
	var st State = StateUnknown
	err := a.do(func(conn snmpConn) error {
		resp, err := conn.Get([]string{a.stOID})
		if err != nil {
			return fmt.Errorf("power: snmp get %s: %w", a.stOID, err)
		}
		if len(resp.Variables) == 0 {
			return fmt.Errorf("power: snmp get %s returned no variables", a.stOID)
		}
		v := resp.Variables[0]
		n, ok := asInt(v.Value)
		if !ok {
			return fmt.Errorf("power: unexpected outlet state value %v", v.Value)
		}
		switch n {
		case stateOn:
			st = StateOn
		case stateOff:
			st = StateOff
		default:
			st = StateUnknown
		}
		return nil
	})
	return st, err
}

// Cycle implements Controller as an explicit off/wait/on so the board fully
// power-cycles regardless of the PDU's configured reboot duration.
func (a *APC) Cycle(ctx context.Context, offFor time.Duration) error {
	if offFor <= 0 {
		offFor = a.cfg.OffDuration
	}
	return cycle(ctx, a, offFor, nil)
}

// Close implements Controller. Connections are per-request, so nothing to do.
func (a *APC) Close() error { return nil }

func asInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case uint:
		return int(n), true
	case uint64:
		return int(n), true
	case uint32:
		return int(n), true
	}
	return 0, false
}

func splitHostPort(addr string, defPort uint16) (string, uint16) {
	// Minimal host[:port] parse that tolerates bare hosts and IPv6 in brackets.
	if addr == "" {
		return addr, defPort
	}
	if addr[0] == '[' {
		if i := indexByte(addr, ']'); i >= 0 {
			host := addr[1:i]
			rest := addr[i+1:]
			if len(rest) > 1 && rest[0] == ':' {
				if p, ok := parsePort(rest[1:]); ok {
					return host, p
				}
			}
			return host, defPort
		}
	}
	if i := lastIndexByte(addr, ':'); i >= 0 && indexByte(addr, ':') == i {
		if p, ok := parsePort(addr[i+1:]); ok {
			return addr[:i], p
		}
	}
	return addr, defPort
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func parsePort(s string) (uint16, bool) {
	n := 0
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
		if n > 65535 {
			return 0, false
		}
	}
	return uint16(n), true
}
