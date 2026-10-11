package ingest

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertkoller/engrex/internal/chunker"
)

const drawingNamespaces = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" ` +
	`xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" ` +
	`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`

// writePPTX zips the given parts (entry name -> XML) into a .pptx and returns its path
func writePPTX(t *testing.T, parts map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deck.pptx")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	archive := zip.NewWriter(file)
	for name, content := range parts {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// slideXML builds a slide (or notes) part with one <a:p> per paragraph
func slideXML(paragraphs ...string) string {
	var builder strings.Builder
	builder.WriteString(`<p:sld ` + drawingNamespaces + `><p:cSld><p:spTree><p:sp><p:txBody>`)
	for _, paragraph := range paragraphs {
		builder.WriteString(`<a:p><a:r><a:t>` + paragraph + `</a:t></a:r></a:p>`)
	}
	builder.WriteString(`</p:txBody></p:sp></p:spTree></p:cSld></p:sld>`)
	return builder.String()
}

func relationshipsXML(relationships ...string) string {
	return `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		strings.Join(relationships, "") + `</Relationships>`
}

func TestExtractPPTXFollowsPresentationOrderAndNotes(t *testing.T) {
	// slide2.xml is shown first, the way it is after reordering a deck in PowerPoint
	parts := map[string]string{
		"ppt/presentation.xml": `<p:presentation ` + drawingNamespaces + `><p:sldIdLst>` +
			`<p:sldId id="256" r:id="rId3"/><p:sldId id="257" r:id="rId2"/>` +
			`</p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": relationshipsXML(
			`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/>`,
			`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide2.xml"/>`,
		),
		"ppt/slides/slide1.xml": slideXML("Second title", "Second body"),
		"ppt/slides/slide2.xml": slideXML("First title", "First body"),
		"ppt/slides/_rels/slide2.xml.rels": relationshipsXML(
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesSlide" Target="../notesSlides/notesSlide7.xml"/>`,
		),
		"ppt/notesSlides/notesSlide7.xml": `<p:notes ` + drawingNamespaces + `><p:cSld><p:spTree><p:sp><p:txBody>` +
			`<a:p><a:r><a:t>Remember the demo</a:t></a:r></a:p>` +
			`<a:p><a:fld type="slidenum"><a:t>1</a:t></a:fld></a:p>` +
			`</p:txBody></p:sp></p:spTree></p:cSld></p:notes>`,
	}
	path := writePPTX(t, parts)

	text, err := ExtractText(path)
	if err != nil {
		t.Fatalf("ExtractText: %v", err)
	}

	want := "# Slide 1\n\nFirst title\nFirst body\n\nSpeaker notes: Remember the demo\n\n" +
		"# Slide 2\n\nSecond title\nSecond body"
	if text != want {
		t.Errorf("got:\n%q\nwant:\n%q", text, want)
	}
}

func TestExtractPPTXFallsBackToNumericOrder(t *testing.T) {
	parts := map[string]string{
		"ppt/slides/slide10.xml": slideXML("Tenth slide here"),
		"ppt/slides/slide2.xml":  slideXML("Second slide here"),
		"ppt/slides/slide1.xml":  slideXML("First slide here"),
	}
	path := writePPTX(t, parts)

	text, err := ExtractText(path)
	if err != nil {
		t.Fatalf("ExtractText: %v", err)
	}

	first := strings.Index(text, "First slide")
	second := strings.Index(text, "Second slide")
	tenth := strings.Index(text, "Tenth slide")
	if first < 0 || second < 0 || tenth < 0 || !(first < second && second < tenth) {
		t.Errorf("slides out of order (lexical sort?):\n%s", text)
	}
}

func TestPPTXChunksPerSlide(t *testing.T) {
	if ContentType("/tmp/deck.pptx") != chunker.TypeMarkdown {
		t.Fatalf(".pptx should chunk as markdown, got %q", ContentType("/tmp/deck.pptx"))
	}
	if !IsSupported("deck.PPTX") {
		t.Error(".pptx not supported")
	}

	path := writePPTX(t, map[string]string{
		"ppt/slides/slide1.xml": slideXML("Quarterly revenue grew twelve percent."),
		"ppt/slides/slide2.xml": slideXML("Hiring is paused until March."),
	})
	text, err := ExtractText(path)
	if err != nil {
		t.Fatalf("ExtractText: %v", err)
	}
	pieces, err := chunker.ChunkDocument(text, chunker.TypeMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(pieces) != 2 || pieces[0].HeadingPath != "Slide 1" || pieces[1].HeadingPath != "Slide 2" {
		t.Errorf("expected one chunk per slide, got %+v", pieces)
	}
}

func TestCleanMarkdownStripsGoogleDocsImages(t *testing.T) {
	input := "# Plan\n\nSee the chart ![][image1] below.\n\n" +
		"[image1]: <data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB>\n"

	output := CleanMarkdown(input)

	if strings.Contains(output, "base64") || strings.Contains(output, "image1") {
		t.Errorf("image data left in output: %q", output)
	}
	if !strings.Contains(output, "# Plan") || !strings.Contains(output, "See the chart  below.") {
		t.Errorf("text or heading lost: %q", output)
	}
}
