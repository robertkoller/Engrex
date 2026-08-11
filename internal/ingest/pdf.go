package ingest

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

const (
	pdfPoolTimeout = 5 * time.Minute

	pdfInstanceLabel = "pdfium"
)

var (
	pdfPoolOnce sync.Once
	pdfPool     pdfium.Pool
	pdfPoolErr  error
)

func pdfiumPool() (pdfium.Pool, error) {
	pdfPoolOnce.Do(func() {
		pdfPool, pdfPoolErr = webassembly.Init(webassembly.Config{
			MinIdle:  1,
			MaxIdle:  1,
			MaxTotal: 1,
		})
	})
	return pdfPool, pdfPoolErr
}

func extractPDF(path string) (string, error) {
	pool, err := pdfiumPool()
	if err != nil {
		return "", fmt.Errorf("starting %s: %w", pdfInstanceLabel, err)
	}

	instance, err := pool.GetInstance(pdfPoolTimeout)
	if err != nil {
		return "", fmt.Errorf("acquiring %s instance: %w", pdfInstanceLabel, err)
	}
	defer instance.Close() //nolint:errcheck

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	document, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		return "", err
	}
	defer instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: document.Document}) //nolint:errcheck

	pageCount, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: document.Document})
	if err != nil {
		return "", err
	}

	var builder strings.Builder
	for pageIndex := range pageCount.PageCount {
		page, err := instance.GetPageText(&requests.GetPageText{
			Page: requests.Page{
				ByIndex: &requests.PageByIndex{Document: document.Document, Index: pageIndex},
			},
		})
		if err != nil {

			continue
		}
		builder.WriteString(page.Text)
	}

	return strings.ReplaceAll(builder.String(), "\r\n", "\n"), nil
}
