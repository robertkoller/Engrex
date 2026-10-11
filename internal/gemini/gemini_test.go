package gemini

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGemini serves the given SSE frames and records the last request
func fakeGemini(t *testing.T, status int, body string) (*Client, *http.Request, *[]byte) {
	t.Helper()
	var captured http.Request
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured = *request
		capturedBody, _ = io.ReadAll(request.Body)
		writer.WriteHeader(status)
		io.WriteString(writer, body) //nolint:errcheck
	}))
	t.Cleanup(server.Close)

	client := New("test-key", "gemini-test")
	client.BaseURL = server.URL
	return client, &captured, &capturedBody
}

func TestStreamSkipsThoughtsAndJoinsParts(t *testing.T) {
	frames := `data: {"candidates":[{"content":{"parts":[{"text":"planning...","thought":true}]}}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":"SQLite lacks "}]}}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":"RIGHT and FULL OUTER joins."}]},"finishReason":"STOP"}]}` + "\n\n"
	client, request, body := fakeGemini(t, http.StatusOK, frames)

	var out strings.Builder
	answer, err := client.Stream("the prompt", &out)
	if err != nil {
		t.Fatal(err)
	}
	want := "SQLite lacks RIGHT and FULL OUTER joins."
	if answer != want || out.String() != want {
		t.Errorf("answer %q, streamed %q, want %q", answer, out.String(), want)
	}

	if request.URL.Path != "/models/gemini-test:streamGenerateContent" || request.URL.Query().Get("alt") != "sse" {
		t.Errorf("wrong endpoint: %s", request.URL)
	}
	if request.Header.Get("x-goog-api-key") != "test-key" || request.URL.Query().Get("key") != "" {
		t.Error("key should go in the header and only there")
	}
	var sent struct {
		Contents []struct {
			Parts []struct{ Text string } `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(*body, &sent); err != nil || sent.Contents[0].Parts[0].Text != "the prompt" {
		t.Errorf("prompt not sent as the user content: %s", *body)
	}
}

func TestStreamReportsAPIErrors(t *testing.T) {
	client, _, _ := fakeGemini(t, http.StatusBadRequest,
		`{"error":{"code":400,"message":"API key not valid.","status":"INVALID_ARGUMENT"}}`)
	_, err := client.Stream("prompt", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "API key not valid.") {
		t.Errorf("got %v", err)
	}
}

func TestStreamFlagsEarlyStopAndBlocks(t *testing.T) {
	client, _, _ := fakeGemini(t, http.StatusOK,
		`data: {"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}]}`+"\n\n")
	var out strings.Builder
	if _, err := client.Stream("prompt", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[Gemini stopped early: MAX_TOKENS]") {
		t.Errorf("early stop not flagged: %q", out.String())
	}

	blocked, _, _ := fakeGemini(t, http.StatusOK, `data: {"promptFeedback":{"blockReason":"SAFETY"}}`+"\n\n")
	if _, err := blocked.Stream("prompt", io.Discard); err == nil || !strings.Contains(err.Error(), "SAFETY") {
		t.Errorf("block not reported: %v", err)
	}
}

func TestStreamNeedsKeyAndIsModel(t *testing.T) {
	if _, err := New("", "gemini-test").Stream("prompt", io.Discard); err == nil {
		t.Error("missing key should fail before any request")
	}
	if !IsModel("gemini-3.8-flash") || IsModel("qwen3:8b") || IsModel("llama3.2") {
		t.Error("IsModel misclassified")
	}
}
