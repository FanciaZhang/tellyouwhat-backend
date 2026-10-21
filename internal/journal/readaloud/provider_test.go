package readaloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validSynthesis() Synthesis {
	return Synthesis{RequestID: uuid.NewString(), Text: "这是一篇合成测试手记。", VoiceID: "zh_female_xiaohe_uranus_bigtts", Style: "natural"}
}

func TestProviderStreamsPCMAndAccountsOnlyFinalUsage(t *testing.T) {
	input := validSynthesis()
	input.VoiceID = "tl_male_jonathan-palad_uranus_bigtts"
	input.Pitch = -2
	input.Style = "calm"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-key" || r.Header.Get("X-Api-Resource-Id") != ResourceID ||
			r.Header.Get("X-Control-Require-Usage-Tokens-Return") != "*" || r.Header.Get("X-Api-Request-Id") != input.RequestID {
			t.Error("missing bounded TTS request headers")
		}
		var body struct {
			Params struct {
				Text, Speaker, Additions string
				Audio                    struct {
					Format                 string
					SampleRate, SpeechRate int
				} `json:"audio_params"`
			} `json:"req_params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Params.Text != input.Text || body.Params.Speaker != input.VoiceID || body.Params.Audio.Format != "pcm" {
			t.Error("text or format changed")
		}
		if !strings.Contains(body.Params.Additions, `"pitch":-2`) || !strings.Contains(body.Params.Additions, "context_texts") {
			t.Error("voice settings missing")
		}
		w.Header().Set("X-Tt-Logid", "synthetic-log")
		fmt.Fprintln(w, `{"code":0,"data":"AQID"}`)
		w.(http.Flusher).Flush()
		fmt.Fprintln(w, `{"code":0,"data":"BA=="}`)
		fmt.Fprintln(w, `{"code":20000000,"data":null,"usage":{"text_words":14}}`)
	}))
	defer server.Close()
	provider, err := NewProvider(server.URL, "synthetic-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var audio bytes.Buffer
	usage, err := provider.Synthesize(context.Background(), input, func(chunk []byte) error { _, err := audio.Write(chunk); return err })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(audio.Bytes(), []byte{1, 2, 3, 4}) || usage.AudioBytes != 4 || !usage.UsageKnown || usage.Characters != 14 || usage.LogID != "synthetic-log" {
		t.Fatalf("invalid streamed result: %+v", usage)
	}
}

func TestProviderRejectsInvalidInputBeforeCallingUpstream(t *testing.T) {
	called := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	provider, _ := NewProvider(server.URL, "synthetic-key", server.Client())
	for _, mutate := range []func(*Synthesis){
		func(x *Synthesis) { x.Text = " " }, func(x *Synthesis) { x.Text = strings.Repeat("字", 301) },
		func(x *Synthesis) { x.Text = string([]byte{0xff}) }, func(x *Synthesis) { x.Pitch = 13 },
		func(x *Synthesis) { x.Style = "user supplied prompt" }, func(x *Synthesis) { x.VoiceID = "../private" },
		func(x *Synthesis) { x.RequestID = "not-a-uuid" }, func(x *Synthesis) { x.Dialect = "unknown" },
	} {
		input := validSynthesis()
		mutate(&input)
		if _, err := provider.Synthesize(context.Background(), input, func([]byte) error { return nil }); !errors.Is(err, ErrInput) {
			t.Errorf("invalid request admitted: %v", err)
		}
	}
	if called {
		t.Fatal("invalid request reached paid provider")
	}
}

func TestProviderRequiresTerminalSuccessAndDoesNotLeakText(t *testing.T) {
	for _, sample := range []struct {
		body string
		want error
	}{
		{`{"code":0,"data":"AQI="}`, ErrIncomplete},
		{`{"code":20000000,"data":null}`, ErrIncomplete},
		{`{"data":"AQI="}`, ErrIncomplete},
		{`{"code":0,"data":"AQ=="}` + "\n" + `{"code":20000000}`, ErrIncomplete},
		{`{"code":45000000,"message":"PRIVATE_TEXT"}`, ErrProvider},
		{`{"code":0,"data":"not-base64"}`, ErrIncomplete},
	} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, sample.body) }))
		provider, _ := NewProvider(server.URL, "synthetic-key", server.Client())
		_, err := provider.Synthesize(context.Background(), validSynthesis(), func([]byte) error { return nil })
		server.Close()
		if !errors.Is(err, sample.want) {
			t.Errorf("wanted %v, got %v", sample.want, err)
		}
		if err != nil && strings.Contains(err.Error(), "PRIVATE_TEXT") {
			t.Fatal("upstream text leaked")
		}
	}
}

func TestProviderCancellationAndConsumerFailureNeverRetry(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintln(w, `{"code":0,"data":"AQI="}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	provider, _ := NewProvider(server.URL, "synthetic-key", server.Client())
	ctx, cancel := context.WithCancel(context.Background())
	_, err := provider.Synthesize(ctx, validSynthesis(), func([]byte) error { cancel(); return context.Canceled })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancel was retried: %v %d", err, calls)
	}
}

func TestProviderDoesNotFollowRedirectWithCredentials(t *testing.T) {
	leaked := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	provider, _ := NewProvider(server.URL, "synthetic-key", server.Client())
	_, err := provider.Synthesize(context.Background(), validSynthesis(), func([]byte) error { return nil })
	if !errors.Is(err, ErrProvider) || leaked {
		t.Fatal("provider redirect forwarded credentials")
	}
}

func TestOfficialVoiceIdentifiersKeepHyphensWithoutAdmittingPaths(t *testing.T) {
	for _, id := range []string{"tl_male_jonathan-palad_uranus_bigtts", "pt_male_eduardo-s_uranus_bigtts", "en_male_adam-imitation_uranus_bigtts"} {
		input := validSynthesis()
		input.VoiceID = id
		voice := draftVoice()
		voice.ID = id
		if !input.Valid() || voice.Validate() != nil {
			t.Errorf("official identifier rejected: %s", id)
		}
	}
	for _, id := range []string{"", "../voice", "voice/name", "voice?key=value", "voice name", "声音", strings.Repeat("a", 129)} {
		input := validSynthesis()
		input.VoiceID = id
		voice := draftVoice()
		voice.ID = id
		if input.Valid() || voice.Validate() == nil {
			t.Errorf("unsafe identifier admitted: %q", id)
		}
	}
}
