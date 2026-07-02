// Package serialport opens a physical serial device as an io.ReadWriteCloser
// for the console manager. It is separated from serialconsole so the console
// logic stays testable without real hardware.
package serialport

import (
	"fmt"
	"io"

	"go.bug.st/serial"
)

// Open opens device at the given baud rate with 8N1 framing (the vEdge 1000
// console settings). The returned value is owned by the caller and must be
// closed.
func Open(device string, baud int) (io.ReadWriteCloser, error) {
	if device == "" {
		return nil, fmt.Errorf("serialport: device path required")
	}
	if baud <= 0 {
		baud = 115200
	}
	mode := &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(device, mode)
	if err != nil {
		return nil, fmt.Errorf("serialport: open %s @ %d: %w", device, baud, err)
	}
	return port, nil
}
