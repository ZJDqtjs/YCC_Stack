package vm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// BuildSeedISO builds the tiny CD-ROM cloud-init expects for the NoCloud
// datasource: an ISO9660 image with the volume label "cidata" and three files
// (user-data, meta-data, network-config) whose names survive because the image
// carries a Joliet directory alongside the ISO one.
//
// The alternative would be shelling out to genisoimage/cloud-localds, but the
// provider agent is supposed to be a single binary that runs on a machine with
// nothing installed. The format written here is the minimum subset Linux's
// isofs and cloud-init actually read.
func BuildSeedISO(files map[string][]byte) ([]byte, error) {
	if len(files) == 0 {
		return nil, errors.New("seed 至少需要一个文件")
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 16 {
		return nil, errors.New("seed 文件过多")
	}
	for _, n := range names {
		if n == "" || len(n) > 32 || strings.ContainsAny(n, "/\\") {
			return nil, fmt.Errorf("seed 文件名无效: %q", n)
		}
	}

	const (
		pvdLBA     = 16
		svdLBA     = 17
		termLBA    = 18
		lPathLBA   = 19
		mPathLBA   = 20
		jlPathLBA  = 21
		jmPathLBA  = 22
		isoRoot    = 23
		jolietRoot = 24
	)
	filesOut := make([]isoFile, 0, len(names))
	cursor := uint32(25)
	for _, n := range names {
		data := files[n]
		sectors := uint32((len(data) + isoSector - 1) / isoSector)
		if sectors == 0 {
			sectors = 1
		}
		filesOut = append(filesOut, isoFile{
			name:       n,
			data:       data,
			extent:     cursor,
			isoName:    isoIdentifier(n),
			jolietName: jolietIdentifier(n),
		})
		cursor += sectors
	}
	total := cursor
	out := make([]byte, int(total)*isoSector)

	writePVD(out[pvdLBA*isoSector:], total, isoRoot, lPathLBA, mPathLBA, false)
	writePVD(out[svdLBA*isoSector:], total, jolietRoot, jlPathLBA, jmPathLBA, true)
	writeTerminator(out[termLBA*isoSector:])
	copy(out[lPathLBA*isoSector:], pathTable(isoRoot, false))
	copy(out[mPathLBA*isoSector:], pathTable(isoRoot, true))
	copy(out[jlPathLBA*isoSector:], pathTable(jolietRoot, false))
	copy(out[jmPathLBA*isoSector:], pathTable(jolietRoot, true))

	isoDir := buildRootDir(filesOut, false, isoRoot)
	jolietDir := buildRootDir(filesOut, true, jolietRoot)
	if len(isoDir) > isoSector || len(jolietDir) > isoSector {
		return nil, errors.New("seed 目录超过一个扇区")
	}
	copy(out[isoRoot*isoSector:], isoDir)
	copy(out[jolietRoot*isoSector:], jolietDir)

	for _, f := range filesOut {
		copy(out[int(f.extent)*isoSector:], f.data)
	}
	return out, nil
}

const isoSector = 2048

type isoFile struct {
	name       string
	data       []byte
	extent     uint32
	isoName    []byte
	jolietName []byte
}

func writePVD(p []byte, total uint32, rootLBA, lPath, mPath uint32, joliet bool) {
	p[0] = 1
	copy(p[1:6], "CD001")
	p[6] = 1
	if joliet {
		p[0] = 2
		// "%/@" selects UCS-2 level 3 in the Joliet supplementary descriptor.
		p[88], p[89], p[90] = '%', '/', '@'
	}
	copy(p[8:40], padded("STARSTACK", 32))
	if joliet {
		copy(p[40:72], ucs2Padded("cidata", 16))
	} else {
		copy(p[40:72], padded("cidata", 32))
	}
	bothEndian32(p[80:88], total)
	bothEndian16(p[120:124], 1)
	bothEndian16(p[124:128], 1)
	bothEndian16(p[128:132], isoSector)
	bothEndian32(p[132:140], 10) // one path-table entry
	le32(p[140:144], lPath)
	le32(p[144:148], 0)
	be32(p[148:152], mPath)
	be32(p[152:156], 0)
	dirRecord(p[156:190], []byte{0}, rootLBA, isoSector, true, fixedTime())
	for _, span := range [][2]int{{190, 318}, {318, 446}, {446, 574}, {574, 702}, {702, 739}, {739, 776}, {776, 813}} {
		copy(p[span[0]:span[1]], padded("", span[1]-span[0]))
	}
	now := fixedTime()
	date17(p[813:830], now)
	date17(p[830:847], now)
	copy(p[847:864], "0000000000000000\x00")
	copy(p[864:881], "0000000000000000\x00")
	p[881] = 1
}

func writeTerminator(p []byte) {
	p[0] = 255
	copy(p[1:6], "CD001")
	p[6] = 1
}

func buildRootDir(files []isoFile, joliet bool, rootLBA uint32) []byte {
	p := make([]byte, isoSector)
	n := 0
	n += dirRecord(p[n:], []byte{0}, rootLBA, isoSector, true, fixedTime())
	n += dirRecord(p[n:], []byte{1}, rootLBA, isoSector, true, fixedTime())
	for _, f := range files {
		id := f.isoName
		if joliet {
			id = f.jolietName
		}
		n += dirRecord(p[n:], id, f.extent, uint32(len(f.data)), false, fixedTime())
	}
	return p
}

func dirRecord(dst, ident []byte, extent, size uint32, dir bool, t time.Time) int {
	l := 33 + len(ident)
	if l%2 == 1 {
		l++
	}
	d := dst[:l]
	d[0] = byte(l)
	d[1] = 0
	bothEndian32(d[2:10], extent)
	bothEndian32(d[10:18], size)
	date7(d[18:25], t)
	if dir {
		d[25] = 2
	}
	bothEndian16(d[28:32], 1)
	d[32] = byte(len(ident))
	copy(d[33:], ident)
	return l
}

func pathTable(root uint32, bigEndian bool) []byte {
	p := make([]byte, isoSector)
	p[0] = 1
	p[1] = 0
	if bigEndian {
		be32(p[2:6], root)
		binary.BigEndian.PutUint16(p[6:8], 1)
	} else {
		le32(p[2:6], root)
		binary.LittleEndian.PutUint16(p[6:8], 1)
	}
	p[8] = 0
	return p
}

func isoIdentifier(name string) []byte {
	up := strings.ToUpper(name)
	var b []byte
	for _, r := range up {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			b = append(b, byte(r))
		} else {
			b = append(b, '_')
		}
	}
	if len(b) > 30 {
		b = b[:30]
	}
	out := append(b, ';', '1')
	return out
}

func jolietIdentifier(name string) []byte {
	u := utf16.Encode([]rune(name))
	b := make([]byte, 0, len(u)*2)
	for _, r := range u {
		b = append(b, byte(r>>8), byte(r))
	}
	return b
}

func padded(s string, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	copy(b, s)
	return b
}

func ucs2Padded(s string, chars int) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, chars*2)
	for i := 0; i < chars; i++ {
		var r uint16 = ' '
		if i < len(u) {
			r = u[i]
		}
		b[i*2], b[i*2+1] = byte(r>>8), byte(r)
	}
	return b
}

func bothEndian16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func bothEndian32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}

func le32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func be32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

func date7(b []byte, t time.Time) {
	t = t.UTC()
	b[0] = byte(t.Year() - 1900)
	b[1] = byte(t.Month())
	b[2] = byte(t.Day())
	b[3] = byte(t.Hour())
	b[4] = byte(t.Minute())
	b[5] = byte(t.Second())
	b[6] = 0
}

func date17(b []byte, t time.Time) {
	t = t.UTC()
	copy(b, t.Format("20060102150405")+"00")
	b[16] = 0
}

// fixedTime keeps the image reproducible: two seeds built from the same files
// at different seconds must be byte-identical, otherwise a test cannot compare
// them and a rebuilt VM looks like it changed for no reason.
func fixedTime() time.Time { return time.Unix(0, 0).UTC() }
