package power

import (
	"context"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

// mockConn records Sets and answers Gets from a scripted state value.
type mockConn struct {
	sets      []gosnmp.SnmpPDU
	gets      []string
	stateVal  int
	connected bool
}

func (m *mockConn) Connect() error { m.connected = true; return nil }
func (m *mockConn) Close() error   { return nil }
func (m *mockConn) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	m.gets = append(m.gets, oids...)
	return &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{
		Name: oids[0], Type: gosnmp.Integer, Value: m.stateVal,
	}}}, nil
}
func (m *mockConn) Set(pdus []gosnmp.SnmpPDU) (*gosnmp.SnmpPacket, error) {
	m.sets = append(m.sets, pdus...)
	return &gosnmp.SnmpPacket{Error: gosnmp.NoError}, nil
}

func newTestAPC(t *testing.T, outlet int, mock *mockConn) *APC {
	t.Helper()
	a, err := NewAPC(APCConfig{Address: "10.0.0.1", Community: "private", Outlet: outlet})
	if err != nil {
		t.Fatalf("NewAPC: %v", err)
	}
	a.dialer = func() (snmpConn, error) { return mock, nil }
	return a
}

func TestAPCOnOffOIDs(t *testing.T) {
	mock := &mockConn{}
	a := newTestAPC(t, 3, mock)
	ctx := context.Background()

	if err := a.On(ctx); err != nil {
		t.Fatalf("On: %v", err)
	}
	if err := a.Off(ctx); err != nil {
		t.Fatalf("Off: %v", err)
	}
	if len(mock.sets) != 2 {
		t.Fatalf("want 2 sets, got %d", len(mock.sets))
	}
	wantOID := "1.3.6.1.4.1.318.1.1.12.3.3.1.1.4.3"
	if mock.sets[0].Name != wantOID || mock.sets[0].Value != cmdImmediateOn {
		t.Fatalf("On set = %+v, want %s=%d", mock.sets[0], wantOID, cmdImmediateOn)
	}
	if mock.sets[1].Value != cmdImmediateOff {
		t.Fatalf("Off value = %v, want %d", mock.sets[1].Value, cmdImmediateOff)
	}
}

func TestAPCStatus(t *testing.T) {
	mock := &mockConn{stateVal: stateOn}
	a := newTestAPC(t, 3, mock)
	st, err := a.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st != StateOn {
		t.Fatalf("state = %v, want on", st)
	}
	if len(mock.gets) != 1 || mock.gets[0] != "1.3.6.1.4.1.318.1.1.12.3.5.1.1.4.3" {
		t.Fatalf("unexpected gets: %v", mock.gets)
	}

	mock.stateVal = stateOff
	st, _ = a.Status(context.Background())
	if st != StateOff {
		t.Fatalf("state = %v, want off", st)
	}
}

func TestAPCCycle(t *testing.T) {
	mock := &mockConn{}
	a := newTestAPC(t, 3, mock)
	if err := a.Cycle(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	// Cycle should issue Off then On.
	if len(mock.sets) != 2 {
		t.Fatalf("want 2 sets, got %d", len(mock.sets))
	}
	if mock.sets[0].Value != cmdImmediateOff || mock.sets[1].Value != cmdImmediateOn {
		t.Fatalf("cycle order wrong: %v", mock.sets)
	}
}

func TestNewAPCValidation(t *testing.T) {
	if _, err := NewAPC(APCConfig{Outlet: 3}); err == nil {
		t.Fatal("expected error for missing address")
	}
	if _, err := NewAPC(APCConfig{Address: "x", Outlet: 0}); err == nil {
		t.Fatal("expected error for invalid outlet")
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port uint16
	}{
		{"10.0.0.1", "10.0.0.1", 161},
		{"10.0.0.1:1161", "10.0.0.1", 1161},
		{"pdu.local", "pdu.local", 161},
		{"[fe80::1]:161", "fe80::1", 161},
		{"[fe80::1]", "fe80::1", 161},
	}
	for _, c := range cases {
		h, p := splitHostPort(c.in, 161)
		if h != c.host || p != c.port {
			t.Errorf("splitHostPort(%q) = %q,%d want %q,%d", c.in, h, p, c.host, c.port)
		}
	}
}

func TestFakeController(t *testing.T) {
	f := NewFake(StateOff)
	ctx := context.Background()
	if err := f.Cycle(ctx, time.Millisecond); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	st, _ := f.Status(ctx)
	if st != StateOn {
		t.Fatalf("state = %v want on", st)
	}
}
