package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"starstack/internal/render"
)

func TestUnzipBundleRejectsEscapingEntry(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "evil.zip")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("../escaped.txt")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("nope"))
	zw.Close()
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unzipBundle(src, dst); err == nil {
		t.Fatal("an entry that escapes the bundle directory must be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); err == nil {
		t.Fatal("the escaping file was written outside the destination")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "escaped.txt")); err == nil {
		t.Fatal("the escaping file was written next to the destination")
	}
}

func TestUnzipBundleKeepsNestedLayout(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bundle.zip")
	f, _ := os.Create(src)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("proj/media/a.mp4")
	w.Write([]byte("video"))
	zw.Close()
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := unzipBundle(src, dst); err != nil {
		t.Fatal(err)
	}
	idx, err := fileIndex(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx["a.mp4"]; !ok {
		t.Fatalf("asset must be found by base name, index=%v", idx)
	}
}

func TestBlenderExprOnlyTouchesRequestedSettings(t *testing.T) {
	if got := blenderExpr(&render.Spec{}); got != "" {
		t.Fatalf("an untouched scene must not get a python override, got %q", got)
	}
	got := blenderExpr(&render.Spec{Samples: 64, Percent: 50, FPS: 24})
	for _, want := range []string{"cycles.samples=64", "resolution_percentage=50", "render.fps=24"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expression %q missing %q", got, want)
		}
	}
	ntsc := blenderExpr(&render.Spec{FPS: 30000.0 / 1001})
	if !strings.Contains(ntsc, "fps_base=") {
		t.Fatalf("NTSC rate must be carried through fps_base, got %q", ntsc)
	}
}
