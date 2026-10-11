package ingest

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// extractPPTX turns a .pptx into markdown with one "# Slide N" section per slide and
// the speaker notes under it, so the chunker keeps each slide as its own chunk.
// Slide order comes from presentation.xml since the slideN.xml file names don't follow
// reordering
func extractPPTX(filePath string) (string, error) {
	reader, err := zip.OpenReader(filePath)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	entries := make(map[string]*zip.File, len(reader.File))
	for _, file := range reader.File {
		entries[file.Name] = file
	}

	slidePaths, err := orderedSlides(entries)
	if err != nil {
		return "", err
	}

	var builder strings.Builder
	for index, slidePath := range slidePaths {
		slideText, err := readSlideText(entries, slidePath)
		if err != nil {
			return "", err
		}
		notesText := ""
		if notesPath := notesFor(entries, slidePath); notesPath != "" {
			notesText, err = readSlideText(entries, notesPath)
			if err != nil {
				return "", err
			}
		}
		if slideText == "" && notesText == "" {
			continue
		}

		fmt.Fprintf(&builder, "# Slide %d\n\n", index+1)
		if slideText != "" {
			builder.WriteString(slideText)
			builder.WriteString("\n\n")
		}
		if notesText != "" {
			builder.WriteString("Speaker notes: ")
			builder.WriteString(notesText)
			builder.WriteString("\n\n")
		}
	}

	return strings.TrimSpace(builder.String()), nil
}

// relationship is one <Relationship> entry in a .rels file
type relationship struct {
	ID     string `xml:"Id,attr"`
	Type   string `xml:"Type,attr"`
	Target string `xml:"Target,attr"`
}

// readRelationships parses a .rels part, returning nil when it doesn't exist
func readRelationships(entries map[string]*zip.File, relsPath string) ([]relationship, error) {
	entry, found := entries[relsPath]
	if !found {
		return nil, nil
	}
	content, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer content.Close()

	var parsed struct {
		Relationships []relationship `xml:"Relationship"`
	}
	if err := xml.NewDecoder(content).Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed.Relationships, nil
}

// relsPathFor returns the .rels part that belongs to a part, e.g.
// ppt/slides/slide1.xml -> ppt/slides/_rels/slide1.xml.rels
func relsPathFor(partPath string) string {
	return path.Join(path.Dir(partPath), "_rels", path.Base(partPath)+".rels")
}

// resolveTarget turns a relationship target (relative to the source part's folder)
// into a zip entry name
func resolveTarget(sourcePart, target string) string {
	if absolute, isAbsolute := strings.CutPrefix(target, "/"); isAbsolute {
		return absolute
	}
	return path.Clean(path.Join(path.Dir(sourcePart), target))
}

var slideNumberRegex = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)

// orderedSlides returns slide part paths in presentation order. Falls back to sorting
// by the number in the file name if presentation.xml can't tell us
func orderedSlides(entries map[string]*zip.File) ([]string, error) {
	const presentationPath = "ppt/presentation.xml"

	if entry, found := entries[presentationPath]; found {
		relationships, err := readRelationships(entries, relsPathFor(presentationPath))
		if err != nil {
			return nil, err
		}
		targets := make(map[string]string, len(relationships))
		for _, rel := range relationships {
			targets[rel.ID] = resolveTarget(presentationPath, rel.Target)
		}

		content, err := entry.Open()
		if err != nil {
			return nil, err
		}
		defer content.Close()

		var presentation struct {
			SlideIDs []struct {
				RelationshipID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
			} `xml:"sldIdLst>sldId"`
		}
		if err := xml.NewDecoder(content).Decode(&presentation); err != nil {
			return nil, err
		}

		var ordered []string
		for _, slideID := range presentation.SlideIDs {
			if target, found := targets[slideID.RelationshipID]; found {
				if _, exists := entries[target]; exists {
					ordered = append(ordered, target)
				}
			}
		}
		if len(ordered) > 0 {
			return ordered, nil
		}
	}

	type numberedSlide struct {
		path   string
		number int
	}
	var slides []numberedSlide
	for name := range entries {
		if match := slideNumberRegex.FindStringSubmatch(name); match != nil {
			number, _ := strconv.Atoi(match[1])
			slides = append(slides, numberedSlide{path: name, number: number})
		}
	}
	// Numeric sort so slide10 lands after slide2
	sort.Slice(slides, func(left, right int) bool {
		return slides[left].number < slides[right].number
	})
	ordered := make([]string, len(slides))
	for index, slide := range slides {
		ordered[index] = slide.path
	}
	return ordered, nil
}

// notesFor finds the speaker notes part linked from a slide, or "" if it has none
func notesFor(entries map[string]*zip.File, slidePath string) string {
	relationships, err := readRelationships(entries, relsPathFor(slidePath))
	if err != nil {
		return ""
	}
	for _, rel := range relationships {
		if strings.HasSuffix(rel.Type, "/notesSlide") {
			target := resolveTarget(slidePath, rel.Target)
			if _, found := entries[target]; found {
				return target
			}
		}
	}
	return ""
}

// readSlideText reads a slide or notes part and returns its text
func readSlideText(entries map[string]*zip.File, partPath string) (string, error) {
	entry, found := entries[partPath]
	if !found {
		return "", nil
	}
	content, err := entry.Open()
	if err != nil {
		return "", err
	}
	defer content.Close()

	text, err := parseDrawingText(content)
	if err != nil {
		return "", fmt.Errorf("%s: %w", partPath, err)
	}
	return text, nil
}

// parseDrawingText pulls the <a:t> runs out of DrawingML, one line per <a:p>.
// Slide number fields are skipped since every notes page has one and it's just noise
func parseDrawingText(source io.Reader) (string, error) {
	decoder := xml.NewDecoder(source)
	var lines []string
	var current strings.Builder
	insideText := false
	insideSlideNumber := false

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "t":
				insideText = true
			case "br":
				current.WriteByte(' ')
			case "fld":
				for _, attribute := range element.Attr {
					if attribute.Name.Local == "type" && attribute.Value == "slidenum" {
						insideSlideNumber = true
					}
				}
			}
		case xml.CharData:
			if insideText && !insideSlideNumber {
				current.Write(element)
			}
		case xml.EndElement:
			switch element.Name.Local {
			case "t":
				insideText = false
			case "fld":
				insideSlideNumber = false
			case "p":
				if line := strings.TrimSpace(current.String()); line != "" {
					lines = append(lines, line)
				}
				current.Reset()
			}
		}
	}

	return strings.Join(lines, "\n"), nil
}
