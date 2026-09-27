package illustration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 12, 8))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestGenerateSingleImageAndConfirmedReference(t *testing.T) {
	data := testPNG(t)
	calls := 0
	p, err := NewProvider("https://example.invalid/api/v3", "test-key", "configured-image-model", transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/v3/images/generations" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatal("incorrect provider request")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "configured-image-model" || payload["sequential_image_generation"] != "disabled" || payload["watermark"] != true || payload["response_format"] != "b64_json" {
			t.Fatal("incorrect generation policy")
		}
		if payload["image"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data) {
			t.Fatal("reference changed")
		}
		return response(200, `{"data":[{"b64_json":"`+base64.StdEncoding.EncodeToString(data)+`","size":"999x999"}]}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Generate(context.Background(), Input{Prompt: "河岸的水彩小狗", Reference: data})
	if err != nil || calls != 1 || result.Width != 12 || result.Height != 8 || result.MIME != "image/png" || !bytes.Equal(result.Image, data) {
		t.Fatalf("result mismatch: %v", err)
	}
}

func TestGenerateNeverRetriesOrLeaksProviderBody(t *testing.T) {
	for _, status := range []int{302, 429, 500} {
		calls := 0
		p, _ := NewProvider("https://example.invalid/api/v3", "key", "model", transportFunc(func(*http.Request) (*http.Response, error) {
			calls++
			r := response(status, "private provider diagnostic")
			r.Header.Set("Location", "https://elsewhere.invalid/")
			return r, nil
		}))
		_, err := p.Generate(context.Background(), Input{Prompt: "paint"})
		if err == nil || calls != 1 || strings.Contains(err.Error(), "private") {
			t.Fatal("retry, redirect or diagnostic leak")
		}
		if status == 500 && !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatal("uncertain outcome lost")
		}
	}
	p, _ := NewProvider("https://example.invalid", "key", "model", transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("private transport error") }))
	if _, err := p.Generate(context.Background(), Input{Prompt: "paint"}); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("transport outcome lost")
	}
}

func TestGenerateRejectsInvalidResultsAndInput(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":[]}`, `{"data":[{},{}]}`, `{"data":[{"url":"https://private.invalid"}]}`, `{"data":[{"b64_json":"invalid"}]}`, `{"data":[{"b64_json":"dGV4dA=="}]}`} {
		p, _ := NewProvider("https://example.invalid", "key", "model", transportFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil }))
		if _, err := p.Generate(context.Background(), Input{Prompt: "paint"}); !errors.Is(err, ErrResult) {
			t.Fatalf("accepted invalid result: %v", err)
		}
	}
	p, _ := NewProvider("https://example.invalid", "key", "model", transportFunc(func(*http.Request) (*http.Response, error) { t.Fatal("invalid input sent"); return nil, nil }))
	for _, input := range []Input{{}, {Prompt: "paint", Reference: []byte("bad")}, {Prompt: strings.Repeat("字", 20001)}} {
		if _, err := p.Generate(context.Background(), input); !errors.Is(err, ErrInput) {
			t.Fatal("invalid input accepted")
		}
	}
	for _, endpoint := range []string{"http://example.invalid", "https://user:secret@example.invalid", "https://example.invalid?key=secret"} {
		if _, err := NewProvider(endpoint, "key", "model", nil); !errors.Is(err, ErrInput) {
			t.Fatal("unsafe endpoint accepted")
		}
	}
}
