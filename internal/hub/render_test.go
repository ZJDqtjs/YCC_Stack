package hub

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"starstack/internal/render"
)

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

const demoXMEML = `<?xml version="1.0" encoding="UTF-8"?>
<xmeml version="5">
 <sequence>
  <name>Demo Cut</name>
  <rate><timebase>25</timebase><ntsc>FALSE</ntsc></rate>
  <media>
   <video>
    <format><samplecharacteristics><width>1280</width><height>720</height></samplecharacteristics></format>
    <track>
     <clipitem>
      <name>a</name><start>0</start><end>50</end><in>0</in><out>50</out>
      <rate><timebase>25</timebase><ntsc>FALSE</ntsc></rate>
      <file id="f1"><name>a.mp4</name><pathurl>file://localhost/C:/media/a.mp4</pathurl></file>
     </clipitem>
     <clipitem>
      <name>b</name><start>50</start><end>100</end><in>10</in><out>60</out>
      <rate><timebase>25</timebase><ntsc>FALSE</ntsc></rate>
      <file id="f2"><name>b.mp4</name><pathurl>file://localhost/C:/media/b.mp4</pathurl></file>
     </clipitem>
    </track>
   </video>
  </media>
 </sequence>
</xmeml>`

func TestPlanTimelineNeedsEveryAsset(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	dir := t.TempDir()
	zpath := filepath.Join(dir, "cut.zip")
	writeZip(t, zpath, map[string]string{"cut.xml": demoXMEML, "media/a.mp4": "a", "media/b.mp4": "b"})

	spec, plan, note, err := s.planTimeline(zpath, "cut.zip", 2)
	if err != nil {
		t.Fatalf("planTimeline: %v", err)
	}
	if spec.Kind != render.KindPR || spec.Timeline == nil || spec.Width != 1280 || spec.FPS != 25 {
		t.Fatalf("unexpected spec: %+v", spec)
	}
	if len(plan) != 2 || plan[1].Start != 2 || plan[0].Dur != 2 {
		t.Fatalf("want two 2s slices, got %+v", plan)
	}
	if !strings.Contains(note, "Demo Cut") {
		t.Fatalf("note should name the sequence: %q", note)
	}

	// the same bundle without one referenced file must be refused before billing
	missing := filepath.Join(dir, "missing.zip")
	writeZip(t, missing, map[string]string{"cut.xml": demoXMEML, "media/a.mp4": "a"})
	if _, _, _, err := s.planTimeline(missing, "missing.zip", 2); err == nil ||
		!strings.Contains(err.Error(), "b.mp4") {
		t.Fatalf("want a missing-asset error naming b.mp4, got %v", err)
	}
}

func TestPlanBlenderSplitsFrameRange(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	dir := t.TempDir()
	zpath := filepath.Join(dir, "scene.zip")
	writeZip(t, zpath, map[string]string{
		"proj/scene.blend": "fake", "proj/tex/skin.png": "fake",
	})
	form := map[string][]string{
		"from": {"1"}, "to": {"100"}, "segs": {"4"},
		"engine": {"cycles"}, "samples": {"64"}, "percent": {"50"}, "fps": {"24"},
	}
	spec, plan, _, err := s.planBlender(form, zpath, "scene.zip", 4)
	if err != nil {
		t.Fatalf("planBlender: %v", err)
	}
	if spec.Scene != "scene.blend" || !spec.Bundle || spec.Engine != "CYCLES" {
		t.Fatalf("unexpected spec: %+v", spec)
	}
	if len(plan) != 4 {
		t.Fatalf("want 4 slices, got %d", len(plan))
	}
	total := 0.0
	for _, sl := range plan {
		total += sl.Dur
	}
	if total != 100 {
		t.Fatalf("slices must cover 100 frames, got %v", total)
	}

	// two scenes in one archive are ambiguous: refusing is the only honest answer
	amb := filepath.Join(dir, "two.zip")
	writeZip(t, amb, map[string]string{"a.blend": "x", "b.blend": "y"})
	if _, _, _, err := s.planBlender(map[string][]string{"to": {"10"}}, amb, "two.zip", 2); err == nil ||
		!strings.Contains(err.Error(), "scene") {
		t.Fatalf("want an ambiguity error asking for scene, got %v", err)
	}
}

func TestRenderJobRequirements(t *testing.T) {
	req, vc := jobReq(&Job{Kind: render.KindBlender})
	if req.Runtime != "blender" || vc != "" {
		t.Fatalf("blender jobs must ask for a blender runtime and no codec, got %+v %q", req, vc)
	}
	req, vc = jobReq(&Job{Kind: render.KindPR})
	if req.Runtime != "ffmpeg" || vc != "libx264" {
		t.Fatalf("timeline jobs must ask for ffmpeg/libx264, got %+v %q", req, vc)
	}
	req, _ = jobReq(&Job{Req: Req{Runtime: "python"}, Profile: Profile{VCodec: "libx264"}})
	if req.Runtime != "ffmpeg" {
		t.Fatalf("a transcode must not let the buyer pick the runtime, got %q", req.Runtime)
	}
	req, _ = jobReq(&Job{Profile: Profile{VCodec: "libx264"}})
	if req.Runtime != "" {
		t.Fatalf("an unset runtime must stay compatible with older nodes, got %q", req.Runtime)
	}
	if got := artifactName(render.KindBlender, 7); got != "seg007.zip" {
		t.Fatalf("blender artifacts are frame zips, got %s", got)
	}
	if got := artifactName(render.KindPR, 7); got != "seg007.mkv" {
		t.Fatalf("timeline artifacts are videos, got %s", got)
	}
}
