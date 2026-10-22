package xlsx

import (
	"archive/zip"
	"bytes"
	"testing"
)

// build 組一個最小的 xlsx。
func build(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

const ns = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`

func TestRead(t *testing.T) {
	data := build(t, map[string]string{
		"xl/workbook.xml": `<workbook ` + ns + `><sheets>
			<sheet name="概述" sheetId="1" r:id="rId1"/>
			<sheet name="隱藏" sheetId="2" state="hidden" r:id="rId2"/>
		</sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
			<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
			<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="/xl/worksheets/sheet2.xml"/>
		</Relationships>`,
		"xl/sharedStrings.xml": `<sst ` + ns + `>
			<si><t>部署前檢查清單</t></si>
			<si><r><t>先選</t></r><r><t>環境</t></r><rPh><t>カンキョウ</t></rPh></si>
		</sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet ` + ns + `><sheetData>
			<row r="2"><c r="A2" t="s"><v>0</v></c></row>
			<row r="5"><c r="B5" t="s"><v>1</v></c><c r="C5"><v>0.30000000000000004</v></c><c r="D5" t="inlineStr"><is><t>內嵌</t></is></c><c r="E5" t="e"><v>#REF!</v></c></row>
			<row r="6"><c r="A6" t="s"><v>99</v></c></row>
		</sheetData><drawing r:id="rId1"/></worksheet>`,
		"xl/worksheets/_rels/sheet1.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
			<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/drawing" Target="../drawings/drawing1.xml"/>
		</Relationships>`,
		"xl/drawings/drawing1.xml": `<xdr:wsDr xmlns:xdr="http://schemas.openxmlformats.org/drawingml/2006/spreadsheetDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
			<xdr:twoCellAnchor><xdr:from><xdr:col>3</xdr:col><xdr:row>3</xdr:row></xdr:from><xdr:to><xdr:row>9</xdr:row></xdr:to>
				<xdr:sp><xdr:txBody><a:p><a:r><a:t>點擊後</a:t></a:r><a:r><a:t>進入設定頁</a:t></a:r></a:p><a:p><a:r><a:t>第二段</a:t></a:r></a:p></xdr:txBody></xdr:sp>
			</xdr:twoCellAnchor>
			<xdr:twoCellAnchor><xdr:from><xdr:row>0</xdr:row></xdr:from><xdr:pic/></xdr:twoCellAnchor>
		</xdr:wsDr>`,
		"xl/worksheets/sheet2.xml": `<worksheet ` + ns + `><sheetData><row r="1"><c r="A1" t="str"><v>x</v></c></row></sheetData></worksheet>`,
	})

	wb, err := Read(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.Sheets) != 2 {
		t.Fatalf("got %d sheets", len(wb.Sheets))
	}
	s := wb.Sheets[0]
	if s.Name != "概述" || s.Hidden {
		t.Fatalf("sheet0 = %q hidden=%v", s.Name, s.Hidden)
	}
	want := []Line{
		{Row: 2, Text: "部署前檢查清單"},
		{Row: 4, Text: "點擊後進入設定頁 / 第二段", TextBox: true},
		{Row: 5, Text: "先選環境 | 0.3 | 內嵌"},
	}
	if len(s.Lines) != len(want) {
		t.Fatalf("lines = %+v", s.Lines)
	}
	for i := range want {
		if s.Lines[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, s.Lines[i], want[i])
		}
	}
	if !wb.Sheets[1].Hidden || len(wb.Sheets[1].Lines) != 1 {
		t.Errorf("sheet1 = %+v", wb.Sheets[1])
	}
}

func TestReadNotZip(t *testing.T) {
	if _, err := Read([]byte("version https://git-lfs.github.com/spec/v1")); err == nil {
		t.Fatal("want error for LFS pointer")
	}
}
