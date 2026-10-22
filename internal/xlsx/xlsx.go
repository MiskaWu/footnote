// Package xlsx 從 xlsx 抽出文字：儲存格與文字方塊。只讀需要的 XML，不碰圖片，
// 所以幾百 MB、塞滿截圖的企劃檔也讀得動。
package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

type Workbook struct {
	Sheets []Sheet
}

type Sheet struct {
	Name   string
	Hidden bool
	Lines  []Line // 依列排序；同一列的儲存格以 " | " 串接，文字方塊各自一行
}

// Line 是一列儲存格，或一個文字方塊。Row 從 1 起算，文字方塊取錨點所在的列。
type Line struct {
	Row     int
	Text    string
	TextBox bool
}

const relNS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"

// Read 解析 xlsx 的位元組內容。
func Read(data []byte) (*Workbook, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	read := func(name string) ([]byte, error) {
		f, ok := files[name]
		if !ok {
			return nil, nil
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}

	wbXML, err := read("xl/workbook.xml")
	if err != nil || wbXML == nil {
		return nil, fmt.Errorf("xl/workbook.xml: %v", err)
	}
	var wb struct {
		Sheets []struct {
			Name  string `xml:"name,attr"`
			State string `xml:"state,attr"`
			RID   string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(wbXML, &wb); err != nil {
		return nil, fmt.Errorf("parse workbook: %w", err)
	}
	wbRels, err := readRels(read, "xl/_rels/workbook.xml.rels", "xl")
	if err != nil {
		return nil, err
	}
	shared, err := readShared(read)
	if err != nil {
		return nil, err
	}

	out := &Workbook{}
	for _, s := range wb.Sheets {
		sh := Sheet{Name: s.Name, Hidden: s.State == "hidden" || s.State == "veryHidden"}
		target := wbRels[s.RID].target
		if target == "" {
			out.Sheets = append(out.Sheets, sh)
			continue
		}
		raw, err := read(target)
		if err != nil {
			return nil, fmt.Errorf("sheet %q: %w", s.Name, err)
		}
		if raw != nil {
			lines, err := readSheet(raw, shared)
			if err != nil {
				return nil, fmt.Errorf("sheet %q: %w", s.Name, err)
			}
			sh.Lines = lines
		}
		dir, base := path.Split(target)
		rels, err := readRels(read, dir+"_rels/"+base+".rels", strings.TrimSuffix(dir, "/"))
		if err != nil {
			return nil, err
		}
		for _, r := range rels {
			if !strings.HasSuffix(r.typ, "/drawing") {
				continue
			}
			raw, err := read(r.target)
			if err != nil {
				return nil, err
			}
			if raw != nil {
				sh.Lines = append(sh.Lines, readDrawing(raw)...)
			}
		}
		sort.SliceStable(sh.Lines, func(i, j int) bool { return sh.Lines[i].Row < sh.Lines[j].Row })
		out.Sheets = append(out.Sheets, sh)
	}
	return out, nil
}

type rel struct{ typ, target string }

// readRels 讀 .rels，Target 解析成 zip 內的絕對路徑。
func readRels(read func(string) ([]byte, error), name, baseDir string) (map[string]rel, error) {
	raw, err := read(name)
	if err != nil || raw == nil {
		return map[string]rel{}, err
	}
	var doc struct {
		Rels []struct {
			ID     string `xml:"Id,attr"`
			Type   string `xml:"Type,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	out := map[string]rel{}
	for _, r := range doc.Rels {
		if r.Mode == "External" {
			continue
		}
		t := r.Target
		if strings.HasPrefix(t, "/") {
			t = strings.TrimPrefix(t, "/")
		} else {
			t = path.Clean(path.Join(baseDir, t))
		}
		out[r.ID] = rel{typ: r.Type, target: t}
	}
	return out, nil
}

func readShared(read func(string) ([]byte, error)) ([]string, error) {
	raw, err := read("xl/sharedStrings.xml")
	if err != nil || raw == nil {
		return nil, err
	}
	var doc struct {
		SI []richText `xml:"si"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse sharedStrings: %w", err)
	}
	out := make([]string, len(doc.SI))
	for i, si := range doc.SI {
		out[i] = si.String()
	}
	return out, nil
}

// richText 是 <si> 或 <is>：純文字 <t>，或多段 <r><t>。<rPh>（注音）不收。
type richText struct {
	T *string `xml:"t"`
	R []struct {
		T string `xml:"t"`
	} `xml:"r"`
}

func (r richText) String() string {
	if r.T != nil {
		return *r.T
	}
	var b strings.Builder
	for _, run := range r.R {
		b.WriteString(run.T)
	}
	return b.String()
}

// readSheet 用串流解析，避免大表一次吃進整棵樹。
func readSheet(raw []byte, shared []string) ([]Line, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var lines []Line
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return lines, nil
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "row" {
			continue
		}
		var row struct {
			R     int `xml:"r,attr"`
			Cells []struct {
				Ref string   `xml:"r,attr"`
				T   string   `xml:"t,attr"`
				V   string   `xml:"v"`
				Is  richText `xml:"is"`
			} `xml:"c"`
		}
		if err := dec.DecodeElement(&row, &se); err != nil {
			return nil, err
		}
		var cells []string
		for _, c := range row.Cells {
			var v string
			switch c.T {
			case "s":
				i, err := strconv.Atoi(strings.TrimSpace(c.V))
				if err == nil && i >= 0 && i < len(shared) {
					v = shared[i]
				}
			case "inlineStr":
				v = c.Is.String()
			case "str":
				v = c.V
			case "b":
				if c.V == "1" {
					v = "TRUE"
				} else {
					v = "FALSE"
				}
			case "e":
				// 公式錯誤（#REF! 之類）不收
			default:
				v = formatNumber(c.V)
			}
			if v = strings.TrimSpace(v); v != "" {
				cells = append(cells, v)
			}
		}
		if len(cells) > 0 {
			lines = append(lines, Line{Row: row.R, Text: strings.Join(cells, " | ")})
		}
	}
}

func formatNumber(v string) string {
	v = strings.TrimSpace(v)
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return v
	}
	return strconv.FormatFloat(f, 'g', 12, 64)
}

// readDrawing 抽出文字方塊（形狀內的 <a:t>）。每個錨點一個 Line，Row 取錨點起始列。
func readDrawing(raw []byte) []Line {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var out []Line
	var (
		inAnchor, inFrom, inRow bool
		row                     int
		paras                   []string
		para                    strings.Builder
		inText                  bool
	)
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "twoCellAnchor", "oneCellAnchor", "absoluteAnchor":
				inAnchor, row, paras = true, 0, nil
			case "from":
				inFrom = inAnchor
			case "row":
				inRow = inFrom
			case "t":
				inText = inAnchor
			}
		case xml.CharData:
			if inRow {
				if n, err := strconv.Atoi(strings.TrimSpace(string(t))); err == nil {
					row = n + 1
				}
			}
			if inText {
				para.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "row":
				inRow = false
			case "from":
				inFrom = false
			case "t":
				inText = false
			case "p":
				if s := strings.TrimSpace(para.String()); s != "" {
					paras = append(paras, s)
				}
				para.Reset()
			case "twoCellAnchor", "oneCellAnchor", "absoluteAnchor":
				if len(paras) > 0 {
					out = append(out, Line{Row: row, Text: strings.Join(paras, " / "), TextBox: true})
				}
				inAnchor = false
			}
		}
	}
}
