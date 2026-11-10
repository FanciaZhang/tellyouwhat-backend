package readaloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSubtitlePositionsUseOriginalUTF16AndReplayInAudioOrder(t *testing.T) {
	var first, next subtitle
	if err := json.Unmarshal([]byte(`{"text":"😀今天123。","words":[{"word":"😀","startTime":0,"endTime":0.2},{"word":"今天","startTime":0.2,"endTime":0.8},{"word":"123。","startTime":0.8,"endTime":1.2}]}`), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"text":"今天。","words":[{"word":"今天。","startTime":1.3,"endTime":2}]}`), &next); err != nil {
		t.Fatal(err)
	}
	cues := alignedTimings("😀今天123。今天。", []subtitle{next, first, first})
	if !validTimings(cues, 11) || len(cues) != 4 || cues[1].UTF16Location != 2 || cues[2].UTF16Location != 4 || cues[3].UTF16Location != 8 {
		t.Fatalf("wrong source mapping: %+v", cues)
	}
	if got := alignedTimings("unrelated private text", []subtitle{first}); len(got) != 0 {
		t.Fatal("unmatched subtitles moved the cursor")
	}
	for _, cue := range []Timing{{-1, 10, 0, 1}, {0, 0, 0, 1}, {0, 240001, 0, 1}, {0, 10, 10, 2}} {
		if validTimings([]Timing{cue}, 11) {
			t.Fatalf("invalid cue accepted: %+v", cue)
		}
	}
}

func TestUnsupportedSubtitleVoiceUsesBoundedRealPhraseBoundaries(t *testing.T) {
	input := validSynthesis()
	input.Dialect = "beijing"
	input.Text = strings.Repeat("这是没有标点的合成测试文字", 9)
	var received []Timing
	total, calls := 0, 0
	input.OnTimings = func(cues []Timing) error { received = cues; return nil }
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params struct {
				Text  string `json:"text"`
				Audio struct {
					Subtitle bool `json:"enable_subtitle"`
				} `json:"audio_params"`
			} `json:"req_params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Params.Text) > 120 || body.Params.Audio.Subtitle {
			t.Error("unbounded or unsupported subtitle request")
		}
		calls++
		fmt.Fprintf(w, `{"code":0,"data":%q}`+"\n", base64.StdEncoding.EncodeToString(make([]byte, 48000)))
		fmt.Fprintf(w, `{"code":20000000,"usage":{"text_words":%d}}`, utf8.RuneCountInString(body.Params.Text))
	}))
	defer server.Close()
	provider, _ := NewProvider(server.URL, "synthetic-key", server.Client())
	usage, err := provider.Synthesize(context.Background(), input, func(data []byte) error { total += len(data); return nil })
	if err != nil || calls < 2 || total != calls*48000 || usage.AudioBytes != total || !usage.UsageKnown || usage.Characters != utf8.RuneCountInString(input.Text) {
		t.Fatalf("incorrect phrase accounting: calls=%d %+v %v", calls, usage, err)
	}
	if !validTimings(received, utf16Length(input.Text)) || len(received) != calls || received[len(received)-1].UTF16Location+received[len(received)-1].UTF16Length != utf16Length(input.Text) {
		t.Fatalf("lost text range: %+v", received)
	}
}

func TestSubtitleSupportFallsBackForOtherScriptsAndDialects(t *testing.T) {
	for _, test := range []struct {
		text, language, dialect string
		want                    bool
	}{
		{"今天 walking 123 😀。", "", "", true}, {"English text.", "en", "", true},
		{"今日は", "", "", false}, {"привет", "", "", false}, {"bonjour", "fr", "", false}, {"今天", "zh-cn", "beijing", false},
	} {
		input := validSynthesis()
		input.Text = test.text
		input.Language = test.language
		input.Dialect = test.dialect
		if supportsSubtitles(input) != test.want {
			t.Errorf("incorrect subtitle support for language=%s dialect=%s", test.language, test.dialect)
		}
	}
}
