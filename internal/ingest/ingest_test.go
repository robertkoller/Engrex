package ingest

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDOCX builds a minimal but valid .docx (a zip with word/document.xml) holding the
// given WordprocessingML body, and returns its path.
func writeDOCX(t *testing.T, bodyXML string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.docx")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	archive := zip.NewWriter(file)
	entry, err := archive.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	document := `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>` + bodyXML + `</w:body></w:document>`
	if _, err := entry.Write([]byte(document)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func paragraph(runs ...string) string {
	var builder strings.Builder
	builder.WriteString("<w:p>")
	for _, run := range runs {
		builder.WriteString(`<w:r><w:t xml:space="preserve">` + run + `</w:t></w:r>`)
	}
	builder.WriteString("</w:p>")
	return builder.String()
}

func TestExtractDOCX(t *testing.T) {
	body := paragraph("Hello ", "world.") +
		paragraph("Second paragraph with a", "\ttab and text.")
	path := writeDOCX(t, body)

	text, err := ExtractText(path)
	if err != nil {
		t.Fatalf("ExtractText: %v", err)
	}

	if !strings.Contains(text, "Hello world.") {
		t.Errorf("adjacent runs not joined; got %q", text)
	}
	if !strings.Contains(text, "Second paragraph") {
		t.Errorf("second paragraph missing; got %q", text)
	}
	// Paragraphs should be separated by a newline.
	if !strings.Contains(text, "world.\n") {
		t.Errorf("paragraph break not preserved; got %q", text)
	}
}

func TestParseWordDocumentTabAndBreak(t *testing.T) {
	// Tested at the parser level so the short text isn't dropped by ExtractText's
	// minFileSize guard.
	document := `<w:document xmlns:w="ns"><w:body><w:p>` +
		`<w:r><w:t>A</w:t><w:tab/><w:t>B</w:t><w:br/><w:t>C</w:t></w:r>` +
		`</w:p></w:body></w:document>`
	text, err := parseWordDocument(strings.NewReader(document))
	if err != nil {
		t.Fatalf("parseWordDocument: %v", err)
	}
	if !strings.Contains(text, "A\tB") {
		t.Errorf("tab not preserved; got %q", text)
	}
	if !strings.Contains(text, "B\nC") {
		t.Errorf("line break not preserved; got %q", text)
	}
}

func TestExtractDOCXEmptyIsSkipped(t *testing.T) {
	// A near-empty document falls under minFileSize and should yield "".
	text, err := ExtractText(writeDOCX(t, paragraph("hi")))
	if err != nil {
		t.Fatalf("ExtractText: %v", err)
	}
	if text != "" {
		t.Errorf("tiny doc should be skipped as empty, got %q", text)
	}
}

func TestDOCXIsSupported(t *testing.T) {
	if !IsSupported("/tmp/report.docx") {
		t.Error("IsSupported(.docx) = false, want true")
	}
	if !IsSupported("/tmp/REPORT.DOCX") {
		t.Error("IsSupported is not case-insensitive for .docx")
	}
	if IsSupported("/tmp/legacy.doc") {
		t.Error("IsSupported(.doc) = true, but only .docx is handled")
	}
}
