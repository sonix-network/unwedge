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

func TestStoreRejectsTraversal(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	for _, bad := range []string{"../evil", "a/b", "..", "", `a\b`} {
		if _, err := st.Path(bad); err == nil {
			t.Errorf("expected rejection of %q", bad)
		}
	}
}
