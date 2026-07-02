package serialconsole

import (
	"fmt"
	"strings"
)

// KeyBytes translates a named key into the byte(s) to send over the console.
// Recognized names (case-insensitive):
//
//	enter / return / cr      -> "\r"
//	lf / newline             -> "\n"
//	crlf                     -> "\r\n"
//	esc / escape             -> 0x1b
//	space                    -> ' '
//	tab                      -> '\t'
//	backspace / bs           -> 0x08
//	del / delete             -> 0x7f
//	ctrl-x, ctrl-c, ...      -> the corresponding control byte (0x01..0x1a)
//
// Ctrl-X is how the U-Boot boot sequence is interrupted on the vEdge 1000.
func KeyBytes(name string) ([]byte, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "enter", "return", "cr":
		return []byte{'\r'}, nil
	case "lf", "newline":
		return []byte{'\n'}, nil
	case "crlf":
		return []byte{'\r', '\n'}, nil
	case "esc", "escape":
		return []byte{0x1b}, nil
	case "space":
		return []byte{' '}, nil
	case "tab":
		return []byte{'\t'}, nil
	case "backspace", "bs":
		return []byte{0x08}, nil
	case "del", "delete":
		return []byte{0x7f}, nil
	}
	if strings.HasPrefix(n, "ctrl-") && len(n) == 6 {
		ch := n[5]
		if ch >= 'a' && ch <= 'z' {
			return []byte{ch - 'a' + 1}, nil
		}
	}
	return nil, fmt.Errorf("serialconsole: unknown key %q", name)
}

// KeysBytes concatenates several named keys.
func KeysBytes(names []string) ([]byte, error) {
	var out []byte
	for _, name := range names {
		b, err := KeyBytes(name)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}
