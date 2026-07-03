// Package tftp provides a read-only TFTP server plus an image store, used to
// serve kernel/rootfs images to the target's U-Boot during netboot.
package tftp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Info describes a stored image.
type Info struct {
	Name    string
	Size    int64
	ModTime time.Time
	CRC32   uint32 // IEEE CRC32, matching U-Boot's crc32 command
	SHA256  string // hex, populated by Save; empty in List for speed
}

// Store manages image files in a directory. A non-empty prefix namespaces the
// store: client-facing names are stored on disk with the prefix prepended and
// stripped again on the way out, so several unwedged instances can share one
// image directory without their uploads colliding. The raw (prefix-less) store
// is what the TFTP read server uses, so it serves files by their exact on-disk
// basename regardless of which instance owns them.
type Store struct {
	dir    string
	prefix string
}

// NewStore creates (if needed) and returns a raw (un-namespaced) Store rooted at
// dir. Use Namespaced to derive a per-instance view.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("tftp: image dir required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("tftp: create image dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Namespaced returns a view of the store whose images are transparently
// prefixed with the given instance name, keeping a shared image directory
// collision-free across instances. An empty name returns an un-namespaced view
// (equivalent to the raw store), preserving the legacy on-disk layout.
func (s *Store) Namespaced(name string) *Store {
	prefix := ""
	if name != "" {
		prefix = name + "--"
	}
	return &Store{dir: s.dir, prefix: prefix}
}

// Dir returns the store's directory.
func (s *Store) Dir() string { return s.dir }

// sanitize rejects names that are not plain basenames, preventing traversal.
func sanitize(name string) (string, error) {
	base := filepath.Base(name)
	if name != base || base == "" || base == "." || base == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("tftp: invalid image name %q (must be a plain filename)", name)
	}
	return base, nil
}

// OnDiskName returns the basename a client-facing image is stored under (with
// this store's namespace prefix applied) — i.e. the filename U-Boot must request
// over TFTP from the shared server.
func (s *Store) OnDiskName(name string) (string, error) {
	base, err := sanitize(name)
	if err != nil {
		return "", err
	}
	return s.prefix + base, nil
}

// Path returns the absolute path for a client-facing image, validating the name
// and applying this store's namespace prefix.
func (s *Store) Path(name string) (string, error) {
	base, err := sanitize(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, s.prefix+base), nil
}

// Save writes r to name, computing SHA-256 and CRC32 as it streams. If the file
// exists and overwrite is false it returns an error. The write is atomic: data
// goes to a temp file which is renamed into place on success.
func (s *Store) Save(name string, r io.Reader, overwrite bool) (Info, error) {
	path, err := s.Path(name)
	if err != nil {
		return Info{}, err
	}
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return Info{}, fmt.Errorf("tftp: image %q already exists", name)
		}
	}
	tmp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return Info{}, fmt.Errorf("tftp: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup if we failed before the rename.
		if _, statErr := os.Stat(tmpName); statErr == nil {
			os.Remove(tmpName)
		}
	}()

	sh := sha256.New()
	cr := crc32.NewIEEE()
	mw := io.MultiWriter(tmp, sh, cr)
	n, err := io.Copy(mw, r)
	if err != nil {
		tmp.Close()
		return Info{}, fmt.Errorf("tftp: write image: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Info{}, fmt.Errorf("tftp: close image: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return Info{}, fmt.Errorf("tftp: finalize image: %w", err)
	}
	base, _ := sanitize(name)
	info := Info{
		Name:    base,
		Size:    n,
		ModTime: time.Now(),
		CRC32:   cr.Sum32(),
		SHA256:  hex.EncodeToString(sh.Sum(nil)),
	}
	return info, nil
}

// List returns all images (excluding in-progress uploads), with CRC32 computed.
func (s *Store) List() ([]Info, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("tftp: read image dir: %w", err)
	}
	var out []Info
	for _, e := range entries {
		phys := e.Name()
		if e.IsDir() || strings.HasPrefix(phys, ".upload-") {
			continue
		}
		// A namespaced view lists only its own images (prefix match), returning
		// client-facing names with the prefix stripped. The raw store lists all.
		if s.prefix != "" && !strings.HasPrefix(phys, s.prefix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		crc, _ := s.crc32Physical(phys)
		out = append(out, Info{
			Name:    strings.TrimPrefix(phys, s.prefix),
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
			CRC32:   crc,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// crc32Physical computes the IEEE CRC32 of a stored image by its on-disk
// basename (prefix already applied).
func (s *Store) crc32Physical(physBase string) (uint32, error) {
	f, err := os.Open(filepath.Join(s.dir, physBase))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cr := crc32.NewIEEE()
	if _, err := io.Copy(cr, f); err != nil {
		return 0, err
	}
	return cr.Sum32(), nil
}

// Delete removes an image.
func (s *Store) Delete(name string) error {
	path, err := s.Path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("tftp: delete image: %w", err)
	}
	return nil
}
