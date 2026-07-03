package tftp

import (
	"bytes"
	"hash/crc32"
	"strings"
	"testing"
)

func TestStoreSaveListDelete(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	data := []byte("fake kernel image contents")
	info, err := st.Save("kernel.bin", bytes.NewReader(data), false)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if info.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", info.Size, len(data))
	}
	wantCRC := crc32.ChecksumIEEE(data)
	if info.CRC32 != wantCRC {
		t.Fatalf("crc = %08x, want %08x", info.CRC32, wantCRC)
	}
	if len(info.SHA256) != 64 {
		t.Fatalf("sha256 len = %d", len(info.SHA256))
	}

	list, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "kernel.bin" {
		t.Fatalf("List = %+v", list)
	}
	if list[0].CRC32 != wantCRC {
		t.Fatalf("List crc = %08x, want %08x", list[0].CRC32, wantCRC)
	}

	if err := st.Delete("kernel.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list, _ = st.List()
	if len(list) != 0 {
		t.Fatalf("expected empty after delete, got %+v", list)
	}
}

func TestStoreNoOverwrite(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	if _, err := st.Save("a.bin", strings.NewReader("x"), false); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := st.Save("a.bin", strings.NewReader("y"), false); err == nil {
		t.Fatal("expected error saving over existing without overwrite")
	}
	if _, err := st.Save("a.bin", strings.NewReader("y"), true); err != nil {
		t.Fatalf("overwrite save: %v", err)
	}
}

// TestNamespacedStore verifies that two instances sharing one directory store
// identically-named images without colliding, that each lists only its own, and
// that the raw store (the TFTP read path) resolves both by on-disk name.
func TestNamespacedStore(t *testing.T) {
	dir := t.TempDir()
	raw, _ := NewStore(dir)
	a := raw.Namespaced("dut1")
	b := raw.Namespaced("dut2")

	if _, err := a.Save("kernel.bin", strings.NewReader("AAAA"), false); err != nil {
		t.Fatalf("a.Save: %v", err)
	}
	// Same clean name from a different instance must not clash, even without
	// overwrite.
	if _, err := b.Save("kernel.bin", strings.NewReader("BBBB"), false); err != nil {
		t.Fatalf("b.Save (should not collide with dut1): %v", err)
	}

	// On-disk names carry the prefix; that is what U-Boot fetches over TFTP.
	onDisk, err := a.OnDiskName("kernel.bin")
	if err != nil || onDisk != "dut1--kernel.bin" {
		t.Fatalf("a.OnDiskName = %q, %v", onDisk, err)
	}

	// Each namespaced view lists only its own image, by the clean name.
	al, _ := a.List()
	if len(al) != 1 || al[0].Name != "kernel.bin" {
		t.Fatalf("a.List = %+v", al)
	}
	if al[0].CRC32 != crc32.ChecksumIEEE([]byte("AAAA")) {
		t.Fatalf("a crc mismatch: %08x", al[0].CRC32)
	}
	bl, _ := b.List()
	if len(bl) != 1 || bl[0].Name != "kernel.bin" {
		t.Fatalf("b.List = %+v", bl)
	}
	if bl[0].CRC32 != crc32.ChecksumIEEE([]byte("BBBB")) {
		t.Fatalf("b crc mismatch: %08x", bl[0].CRC32)
	}

	// The raw store (used by the TFTP read server) sees both, by on-disk name.
	rl, _ := raw.List()
	if len(rl) != 2 {
		t.Fatalf("raw.List = %+v (want both on-disk files)", rl)
	}
	if _, err := raw.Path("dut1--kernel.bin"); err != nil {
		t.Fatalf("raw.Path(on-disk name): %v", err)
	}

	// Deleting from one namespace leaves the other intact.
	if err := a.Delete("kernel.bin"); err != nil {
		t.Fatalf("a.Delete: %v", err)
	}
	if al, _ := a.List(); len(al) != 0 {
		t.Fatalf("dut1 not empty after delete: %+v", al)
	}
	if bl, _ := b.List(); len(bl) != 1 {
		t.Fatalf("dut2 image should survive dut1 delete: %+v", bl)
	}
}

// TestUnnamespacedStoreIsLegacyLayout confirms that an empty instance name
// yields the original flat on-disk layout, so upgraded single-instance
// controllers keep serving their existing images.
func TestUnnamespacedStoreIsLegacyLayout(t *testing.T) {
	raw, _ := NewStore(t.TempDir())
	st := raw.Namespaced("")
	if _, err := st.Save("kernel.bin", strings.NewReader("x"), false); err != nil {
		t.Fatalf("Save: %v", err)
	}
	onDisk, _ := st.OnDiskName("kernel.bin")
	if onDisk != "kernel.bin" {
		t.Fatalf("OnDiskName = %q, want unprefixed", onDisk)
	}
	// The raw store must find it at the bare name (what U-Boot requested pre-upgrade).
	if _, err := raw.Path("kernel.bin"); err != nil {
		t.Fatalf("raw.Path: %v", err)
	}
}

func TestStoreRejectsTraversal(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	for _, bad := range []string{"../evil", "a/b", "..", "", `a\b`} {
		if _, err := st.Path(bad); err == nil {
			t.Errorf("expected rejection of %q", bad)
		}
	}
}
