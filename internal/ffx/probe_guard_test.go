package ffx

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// A playlist is the difference between "read this file" and "read whatever this
// file names". Probe must accept ordinary media and reject the playlist before
// the demuxer can follow an absolute path or an HTTP URL.
func TestProbeRejectsPlaylist(t *testing.T) {
	if _, err := exec.LookPath(Ffmpeg); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	mp4 := filepath.Join(dir, "ok.mp4")
	if out, err := exec.Command(Ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=24", "-t", "1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", mp4).CombinedOutput(); err != nil {
		t.Fatalf("make fixture: %v %s", err, out)
	}
	if _, err := Probe(context.Background(), mp4); err != nil {
		t.Fatalf("ordinary mp4 must still probe: %v", err)
	}

	playlist := filepath.Join(dir, "evil.m3u8")
	if out, err := exec.Command(Ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-i", mp4, "-c", "copy", "-f", "hls", "-hls_time", "1", "-hls_list_size", "0",
		"-hls_segment_filename", filepath.Join(dir, "seg%d.ts"), playlist).CombinedOutput(); err != nil {
		t.Fatalf("make playlist: %v %s", err, out)
	}
	if _, err := Probe(context.Background(), playlist); err == nil {
		t.Fatal("HLS playlist must be rejected by the input guard")
	}
}
