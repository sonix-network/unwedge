package sshexec

import (
	"fmt"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func knownHostsCallback(path string) (ssh.HostKeyCallback, error) {
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("sshexec: load known_hosts %s: %w", path, err)
	}
	return cb, nil
}
