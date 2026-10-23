package render

import (
	"math"
	"testing"
)

const xmemlFixture = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE xmeml>
<xmeml version="4">
 <sequence id="sequence-1">
  <name>毕业短片 第一幕</name>
  <duration>300</duration>
  <rate><timebase>25</timebase><ntsc>FALSE</ntsc></rate>
  <media>
   <video>
    <format><samplecharacteristics><width>1920</width><height>1080</height></samplecharacteristics></format>
    <track>
     <clipitem id="clipitem-1">
      <name>a.mp4</name><duration>150</duration>
      <rate><timebase>25</timebase><ntsc>FALSE</ntsc></rate>
      <start>0</start><end>100</end><in>50</in><out>150</out>
      <file id="file-1"><name>a.mp4</name>
        <pathurl>file://localhost/D:/media/a.mp4</pathurl>
        <rate><timebase>25</timebase><ntsc>FALSE</ntsc></rate></file>
     </clipitem>
     <clipitem id="clipitem-2">
      <name>b.mp4</name><duration>120</duration>
      <rate><timebase>25</timebase><ntsc>FALSE</ntsc></rate>
      <start>100</start><end>220</end><in>0</in><out>120</out>
      <file id="file-2"><name>b.mp4</name>
        <pathurl>file://localhost/D:/media/b%20roll.mp4</pathurl>
        <rate><timebase>25</timebase><ntsc>FALSE</ntsc></rate></file>
     </clipitem>
    </track>
   </video>
  </media>
 </sequence>
</xmeml>`

func TestParseXMEML(t *testing.T) {
	tl, err := ParseTimeline([]byte(xmemlFixture))
	if err != nil {
		t.Fatal(err)
	}
	if tl.FPS != 25 || tl.Width != 1920 || tl.Height != 1080 {
		t.Fatalf("format = %.2ffps %dx%d", tl.FPS, tl.Width, tl.Height)
	}
	if len(tl.Clips) != 2 {
		t.Fatalf("clips = %d", len(tl.Clips))
	}
	// 100 timeline frames at 25 fps = 4s; source in 50 frames = 2s
	if got := tl.Clips[0]; math.Abs(got.At-0) > 1e-6 || math.Abs(got.In-2) > 1e-6 || math.Abs(got.Dur-4) > 1e-6 {
		t.Fatalf("clip 0 = %+v", got)
	}
	if got := tl.Clips[1]; math.Abs(got.At-4) > 1e-6 || math.Abs(got.Dur-4.8) > 1e-6 {
		t.Fatalf("clip 1 = %+v", got)
	}
	if got := tl.Clips[1].Asset; got != "b roll.mp4" {
		t.Fatalf("percent-encoded asset = %q", got)
	}
	if tl.Duration != 8.8 {
		t.Fatalf("duration = %v", tl.Duration)
	}
}

const fcpxmlFixture = `<?xml version="1.0" encoding="UTF-8"?>
<fcpxml version="1.9">
 <resources>
  <format id="r1" width="1280" height="720" frameDuration="100/3000s"/>
  <asset id="r2" name="take1" src="file:///Users/me/media/take1.mov"/>
  <asset id="r3" name="take2" src="file:///Users/me/media/take2.mov"/>
 </resources>
 <library><event><project name="预告片"><sequence format="r1" duration="720/30s">
  <spine>
   <asset-clip ref="r2" offset="0s" start="30/30s" duration="240/30s"/>
   <asset-clip ref="r3" offset="240/30s" start="0s" duration="360/30s"/>
  </spine>
 </sequence></project></event></library>
</fcpxml>`

func TestParseFCPXML(t *testing.T) {
	tl, err := ParseTimeline([]byte(fcpxmlFixture))
	if err != nil {
		t.Fatal(err)
	}
	if tl.FPS != 30 || tl.Width != 1280 || tl.Height != 720 {
		t.Fatalf("format = %.2ffps %dx%d", tl.FPS, tl.Width, tl.Height)
	}
	if len(tl.Clips) != 2 {
		t.Fatalf("clips = %d", len(tl.Clips))
	}
	if tl.Clips[0].Asset != "take1.mov" || math.Abs(tl.Clips[0].In-1) > 1e-6 || math.Abs(tl.Clips[0].Dur-8) > 1e-6 {
		t.Fatalf("clip 0 = %+v", tl.Clips[0])
	}
	if math.Abs(tl.Duration-24) > 1e-6 {
		t.Fatalf("duration = %v", tl.Duration)
	}
}

func TestParseRejectsPremiereProject(t *testing.T) {
	// gzip magic is what a .prproj starts with
	_, err := ParseTimeline([]byte{0x1f, 0x8b, 0x08, 0x00})
	if err != ErrPremiereProject {
		t.Fatalf("err = %v", err)
	}
}

// A slice boundary that falls inside a cut must trim the clip on both sides
// without losing or duplicating a single second.
func TestPlanTimelineSplitsInsideClips(t *testing.T) {
	tl := &Timeline{FPS: 25, Duration: 10, Clips: []Clip{
		{Asset: "a.mp4", In: 0, Dur: 4, At: 0},
		{Asset: "b.mp4", In: 0, Dur: 6, At: 4},
	}}
	slices := PlanTimeline(tl, 4)
	if len(slices) != 4 {
		t.Fatalf("slices = %d", len(slices))
	}
	total := 0.0
	for i, s := range slices {
		if math.Abs(s.Start-float64(i)*2.5) > 1e-6 {
			t.Fatalf("slice %d start = %v", i, s.Start)
		}
		sum := 0.0
		for _, c := range s.Clips {
			sum += c.Dur
			if c.At < -1e-9 || c.At+c.Dur > s.Dur+1e-6 {
				t.Fatalf("slice %d clip %+v escapes the slice (dur %v)", i, c, s.Dur)
			}
		}
		if math.Abs(sum-s.Dur) > 1e-6 {
			t.Fatalf("slice %d covers %.6fs of %.6fs", i, sum, s.Dur)
		}
		total += s.Dur
	}
	if math.Abs(total-10) > 1e-6 {
		t.Fatalf("slices cover %v of 10s", total)
	}
	// 2.5s falls inside a.mp4, so slice 0 is a strict prefix of it
	if slices[0].Clips[0].Asset != "a.mp4" || math.Abs(slices[0].Clips[0].Dur-2.5) > 1e-6 {
		t.Fatalf("slice 0 = %+v", slices[0].Clips)
	}
	// the cut at 4s is inside slice 1: its tail of a.mp4 then the head of b.mp4
	if len(slices[1].Clips) != 2 || slices[1].Clips[0].Asset != "a.mp4" || slices[1].Clips[1].Asset != "b.mp4" {
		t.Fatalf("slice 1 = %+v", slices[1].Clips)
	}
	// slice 2 starts 1s into b.mp4 (b begins at 4s, slice at 5s)
	if slices[2].Clips[0].Asset != "b.mp4" || math.Abs(slices[2].Clips[0].In-1) > 1e-6 {
		t.Fatalf("slice 2 starts mid-second clip: %+v", slices[2].Clips[0])
	}
}

func TestPlanFramesCoversEveryFrameOnce(t *testing.T) {
	slices := PlanFrames(1, 100, 4)
	if len(slices) != 4 {
		t.Fatalf("slices = %d", len(slices))
	}
	next := 1.0
	for _, s := range slices {
		if s.Start != next {
			t.Fatalf("frames not contiguous: got %v want %v", s.Start, next)
		}
		next += s.Dur
	}
	if next != 101 {
		t.Fatalf("covered %v frames, want 100", next-1)
	}
}
