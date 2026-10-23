package render

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrPremiereProject is returned for a .prproj. Premiere's own project format is
// gzip-compressed XML with version-specific effect blobs: it describes the edit,
// not the pixels, and no third-party tool can reproduce its renderer. The honest
// route is an interchange export, which Premiere also writes.
var ErrPremiereProject = errors.New("这是 Premiere 工程文件（.prproj）。请先在 Premiere 里「文件 → 导出 → Final Cut Pro XML」，" +
	"把导出的 XML 和素材一起打成 zip 再上传")

// ParseTimeline reads an NLE interchange file: Final Cut Pro 7 XML (xmeml,
// which Premiere exports) or FCPXML. Absolute media paths are reduced to base
// names because the bundle travels to other machines.
func ParseTimeline(data []byte) (*Timeline, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		return nil, ErrPremiereProject
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, errors.New("不是可识别的 XML：读不到根元素")
		}
		if se, ok := tok.(xml.StartElement); ok {
			switch se.Name.Local {
			case "xmeml":
				return parseXMEML(data)
			case "fcpxml":
				return parseFCPXML(data)
			default:
				return nil, fmt.Errorf("不认识的 XML 根元素 <%s>：支持 Premiere 导出的 FCP XML 与 FCPXML", se.Name.Local)
			}
		}
	}
}

// ---- Final Cut Pro 7 XML / Premiere "Final Cut Pro XML" ----

type xRate struct {
	Timebase float64 `xml:"timebase"`
	NTSC     string  `xml:"ntsc"`
}

// fps converts an NLE rate into real frames per second. A 30 fps timebase with
// ntsc=TRUE is 29.97, and frame counts from the file must be divided by that,
// not by the rounded integer, or every cut lands a frame or two early.
func (r xRate) fps() float64 {
	tb := r.Timebase
	if tb <= 0 {
		return 0
	}
	if strings.EqualFold(strings.TrimSpace(r.NTSC), "TRUE") {
		switch int(tb) {
		case 30:
			return 30000.0 / 1001
		case 60:
			return 60000.0 / 1001
		case 24:
			return 24000.0 / 1001
		}
	}
	return tb
}

type xFile struct {
	ID      string `xml:"id,attr"`
	Name    string `xml:"name"`
	PathURL string `xml:"pathurl"`
	Path    string `xml:"path"`
	Rate    xRate  `xml:"rate"`
}

func (f xFile) asset() string {
	if f.PathURL != "" {
		return BaseName(f.PathURL)
	}
	if f.Path != "" {
		return BaseName(f.Path)
	}
	return BaseName(f.Name)
}

type xClip struct {
	Name  string  `xml:"name"`
	Start float64 `xml:"start"`
	End   float64 `xml:"end"`
	In    float64 `xml:"in"`
	Out   float64 `xml:"out"`
	Rate  xRate   `xml:"rate"`
	File  xFile   `xml:"file"`
}

type xSequence struct {
	Name  string `xml:"name"`
	Rate  xRate  `xml:"rate"`
	Media struct {
		Video struct {
			Format struct {
				Sample struct {
					Width  int `xml:"width"`
					Height int `xml:"height"`
				} `xml:"samplecharacteristics"`
			} `xml:"format"`
			Tracks []struct {
				Clips []xClip `xml:"clipitem"`
			} `xml:"track"`
		} `xml:"video"`
	} `xml:"media"`
}

type xDoc struct {
	XMLName   xml.Name
	Sequences []xSequence `xml:"sequence"`
	Project   struct {
		Children struct {
			Sequences []xSequence `xml:"sequence"`
		} `xml:"children"`
	} `xml:"project"`
}

func parseXMEML(data []byte) (*Timeline, error) {
	var doc xDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("FCP XML 解析失败: %v", err)
	}
	seqs := doc.Sequences
	if len(seqs) == 0 {
		seqs = doc.Project.Children.Sequences
	}
	// A Premiere export can carry the whole project; the sequence with the most
	// clips is the one the user was looking at when they exported.
	var seq *xSequence
	best := -1
	for i := range seqs {
		n := 0
		for _, tr := range seqs[i].Media.Video.Tracks {
			n += len(tr.Clips)
		}
		if n > best {
			best, seq = n, &seqs[i]
		}
	}
	if seq == nil || best <= 0 {
		return nil, errors.New("FCP XML 里没有视频片段")
	}
	files := xFileTable(data)
	fps := seq.Rate.fps()
	if fps <= 0 {
		fps = 25
	}
	tl := &Timeline{
		Name:   seq.Name,
		FPS:    fps,
		Width:  seq.Media.Video.Format.Sample.Width,
		Height: seq.Media.Video.Format.Sample.Height,
	}
	if tl.Width == 0 || tl.Height == 0 {
		tl.Width, tl.Height = 1920, 1080
	}
	if len(seq.Media.Video.Tracks) > 1 {
		extra := 0
		for _, tr := range seq.Media.Video.Tracks[1:] {
			extra += len(tr.Clips)
		}
		if extra > 0 {
			tl.Note = fmt.Sprintf("时间线有 %d 条视频轨，只渲染第一条（+%d 个片段未包含）",
				len(seq.Media.Video.Tracks), extra)
		}
	}
	if len(seq.Media.Video.Tracks) == 0 {
		return nil, errors.New("FCP XML 里没有视频轨")
	}
	for _, c := range seq.Media.Video.Tracks[0].Clips {
		// A clipitem may reference a file definition by id instead of inlining it.
		f := c.File
		if f.ID != "" && f.asset() == "" {
			if alt, ok := files[f.ID]; ok {
				f = alt
			}
		}
		asset := f.asset()
		if asset == "" {
			return nil, fmt.Errorf("片段 %q 没有可用的素材路径", c.Name)
		}
		in := c.In
		out := c.Out
		if out <= in {
			return nil, fmt.Errorf("片段 %q 的入点/出点无效 (%g→%g)", c.Name, in, out)
		}
		// in/out are in the clip's own rate; start/end are timeline frames.
		cf := c.Rate.fps()
		if cf <= 0 {
			cf = f.Rate.fps()
		}
		if cf <= 0 {
			cf = fps
		}
		at := c.Start / fps
		dur := (out - in) / cf
		if end := c.End / fps; end > 0 && math.Abs(end-(at+dur)) > 0.05 {
			// start/end is authoritative for the timeline slot; a mismatch means the
			// clip was retimed, which this renderer cannot reproduce
			dur = end - at
			if tl.Note == "" {
				tl.Note = "检测到速度变化/变速片段，按时间线时长渲染"
			}
		}
		if dur <= 0 {
			continue
		}
		tl.Clips = append(tl.Clips, Clip{Asset: asset, In: in / cf, Dur: dur, At: at})
		if end := at + dur; end > tl.Duration {
			tl.Duration = end
		}
	}
	if len(tl.Clips) == 0 {
		return nil, errors.New("FCP XML 里没有可渲染的片段")
	}
	tl.Duration = round(tl.Duration)
	return tl, nil
}

// xFileTable collects <file id="..."> definitions anywhere in the document.
// Decoding the whole tree into typed structs would only see the ones inlined in
// a clipitem, and older exports put them at sequence level.
func xFileTable(data []byte) map[string]xFile {
	out := map[string]xFile{}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "file" {
			continue
		}
		id := ""
		for _, a := range se.Attr {
			if a.Name.Local == "id" {
				id = a.Value
			}
		}
		if id == "" {
			continue
		}
		var f xFile
		if err := dec.DecodeElement(&f, &se); err == nil {
			out[id] = f
		}
	}
}

// ---- FCPXML ----

type fClip struct {
	Ref      string `xml:"ref,attr"`
	Name     string `xml:"name,attr"`
	Offset   string `xml:"offset,attr"`
	Start    string `xml:"start,attr"`
	Duration string `xml:"duration,attr"`
	Lane     string `xml:"lane,attr"`
}

type fDoc struct {
	XMLName   xml.Name
	Resources struct {
		Assets []struct {
			ID   string `xml:"id,attr"`
			Name string `xml:"name,attr"`
			Src  string `xml:"src,attr"`
		} `xml:"asset"`
		Formats []struct {
			ID            string `xml:"id,attr"`
			Width         int    `xml:"width,attr"`
			Height        int    `xml:"height,attr"`
			FrameDuration string `xml:"frameDuration,attr"`
		} `xml:"format"`
	} `xml:"resources"`
	Library struct {
		Event struct {
			Project struct {
				Name     string `xml:"name,attr"`
				Sequence struct {
					Format   string `xml:"format,attr"`
					Duration string `xml:"duration,attr"`
					Spine    struct {
						AssetClips []fClip `xml:"asset-clip"`
						Videos     []fClip `xml:"video"`
					} `xml:"spine"`
				} `xml:"sequence"`
			} `xml:"project"`
		} `xml:"event"`
	} `xml:"library"`
}

// fcpxTime parses FCPXML rational time: "3600/25s", "10s", "0s".
func fcpxTime(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	s = strings.TrimSuffix(s, "s")
	if i := strings.IndexByte(s, '/'); i > 0 {
		num, err1 := strconv.ParseFloat(s[:i], 64)
		den, err2 := strconv.ParseFloat(s[i+1:], 64)
		if err1 == nil && err2 == nil && den != 0 {
			return num / den
		}
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func parseFCPXML(data []byte) (*Timeline, error) {
	var doc fDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("FCPXML 解析失败: %v", err)
	}
	seq := doc.Library.Event.Project.Sequence
	clips := append(append([]fClip{}, seq.Spine.AssetClips...), seq.Spine.Videos...)
	if len(clips) == 0 {
		return nil, errors.New("FCPXML 的 spine 里没有片段")
	}
	tl := &Timeline{Name: doc.Library.Event.Project.Name, FPS: 25}
	for _, f := range doc.Resources.Formats {
		if f.ID == seq.Format {
			if fd := fcpxTime(f.FrameDuration); fd > 0 {
				tl.FPS = math.Round(1/fd*1000) / 1000
			}
			tl.Width, tl.Height = f.Width, f.Height
		}
	}
	if tl.Width == 0 || tl.Height == 0 {
		tl.Width, tl.Height = 1920, 1080
	}
	assets := map[string]string{}
	for _, a := range doc.Resources.Assets {
		src := a.Src
		if src == "" {
			src = a.Name
		}
		assets[a.ID] = BaseName(src)
	}
	lane := 0
	for _, c := range clips {
		if strings.TrimSpace(c.Lane) != "" && c.Lane != "0" {
			lane++
			continue
		}
		asset := assets[c.Ref]
		if asset == "" {
			return nil, fmt.Errorf("片段 %q 引用了不存在的素材 %q", c.Name, c.Ref)
		}
		d := fcpxTime(c.Duration)
		if d <= 0 {
			continue
		}
		at := fcpxTime(c.Offset)
		tl.Clips = append(tl.Clips, Clip{
			Asset: asset, In: fcpxTime(c.Start), Dur: round(d), At: round(at),
		})
		if end := at + d; end > tl.Duration {
			tl.Duration = end
		}
	}
	if len(tl.Clips) == 0 {
		return nil, errors.New("FCPXML 里没有可渲染的片段")
	}
	if want := fcpxTime(seq.Duration); want > tl.Duration+0.05 {
		tl.Duration = round(want)
	}
	if lane > 0 {
		tl.Note = fmt.Sprintf("忽略了 %d 个附加片段（连接片段/多轨叠加）", lane)
	}
	tl.Duration = round(tl.Duration)
	return tl, nil
}
