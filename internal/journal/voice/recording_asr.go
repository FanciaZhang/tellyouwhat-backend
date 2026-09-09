package voice

// Whole-recording ASR is deliberately separate from the low-latency stream.
// Speaker numbers have meaning only inside this provider task.
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const RecordingAnalysisVersion = "journal-recording-v1"
const maxRecordingResponseBytes = 8 << 20

type RecordingUtterance struct {
	ID                string `json:"id"`
	Speaker           string `json:"speaker"`
	StartMilliseconds int    `json:"startMilliseconds"`
	EndMilliseconds   int    `json:"endMilliseconds"`
	Text              string `json:"text"`
	// Preserve provider text exactly; never convert to an emotion enum or score.
	AcousticEmotion string `json:"acousticEmotion,omitempty"`
}
type RecordingAnalysis struct {
	TaskID       string               `json:"taskID"`
	Version      string               `json:"version"`
	Milliseconds int                  `json:"milliseconds"`
	Text         string               `json:"text"`
	Utterances   []RecordingUtterance `json:"utterances"`
}
type RecordingASR struct {
	Config ASRConfig
	Client *http.Client
}
type RecordingProviderError struct {
	Status string
	LogID  string
}

func (e *RecordingProviderError) Error() string { return "recording_provider_" + e.Status }

var ErrRecordingPending = errors.New("recording_analysis_pending")

// The request ID is generated and durably saved by the caller BEFORE submit.
// After a lost response, query this ID instead of creating a second billed task.
func (a RecordingASR) Submit(ctx context.Context, taskID string, wav []byte) error {
	if _, err := uuid.Parse(taskID); err != nil || !validRecordingWAV(wav) {
		return ErrInvalid
	}
	// Stream base64 into the HTTP request instead of holding both 58 MB PCM
	// and its 77 MB JSON copy in the private service's 192 MB memory limit.
	prefix := `{"user":{"uid":"journal-recording"},"audio":{"format":"wav","data":"`
	suffix := `"},"request":{"model_name":"bigmodel","enable_itn":true,"enable_punc":true,"show_utterances":true,"enable_speaker_info":true,"enable_emotion_detection":true}}`
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		_, err := io.WriteString(writer, prefix)
		if err == nil {
			encoder := base64.NewEncoder(base64.StdEncoding, writer)
			_, err = encoder.Write(wav)
			if closeErr := encoder.Close(); err == nil {
				err = closeErr
			}
		}
		if err == nil {
			_, err = io.WriteString(writer, suffix)
		}
		_ = writer.CloseWithError(err)
	}()
	length := int64(len(prefix) + base64.StdEncoding.EncodedLen(len(wav)) + len(suffix))
	_, err := a.callBody(ctx, "submit", taskID, reader, length)
	return err
}
func (a RecordingASR) Query(ctx context.Context, taskID string, milliseconds int) (RecordingAnalysis, error) {
	if _, err := uuid.Parse(taskID); err != nil || milliseconds <= 0 || milliseconds > SessionMilliseconds {
		return RecordingAnalysis{}, ErrInvalid
	}
	data, err := a.call(ctx, "query", taskID, struct{}{})
	if err != nil {
		return RecordingAnalysis{}, err
	}
	return parseRecordingAnalysis(data, taskID, milliseconds)
}
func (a RecordingASR) call(ctx context.Context, action, taskID string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return a.callBody(ctx, action, taskID, bytes.NewReader(data), int64(len(data)))
}
func (a RecordingASR) callBody(ctx context.Context, action, taskID string, body io.Reader, length int64) ([]byte, error) {
	// Never allow a turbo/idle resource to silently replace the validated standard API.
	if a.Config.ResourceID != "volc.seedasr.auc" && a.Config.ResourceID != "volc.bigasr.auc" {
		return nil, ErrInvalid
	}
	base := a.Config.URL
	if base == "" {
		base = "https://openspeech.bytedance.com/api/v3/auc/bigmodel"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/"+action, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Resource-Id", a.Config.ResourceID)
	req.Header.Set("X-Api-Request-Id", taskID)
	req.Header.Set("X-Api-Sequence", "-1")
	if a.Config.APIKey != "" {
		req.Header.Set("X-Api-Key", a.Config.APIKey)
	} else {
		req.Header.Set("X-Api-App-Key", a.Config.AppKey)
		req.Header.Set("X-Api-Access-Key", a.Config.AccessKey)
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	// Redirects must never forward provider credentials, even with a custom client.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := copyClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRecordingResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRecordingResponseBytes {
		return nil, ErrInvalid
	}
	status := response.Header.Get("X-Api-Status-Code")
	if response.StatusCode == 200 && (status == "20000001" || status == "20000002") {
		return nil, ErrRecordingPending
	}
	if response.StatusCode != 200 || status != "20000000" {
		return nil, &RecordingProviderError{Status: status, LogID: response.Header.Get("X-Tt-Logid")}
	}
	return data, nil
}
func parseRecordingAnalysis(data []byte, taskID string, milliseconds int) (RecordingAnalysis, error) {
	var wire struct {
		Audio struct {
			Duration int `json:"duration"`
		} `json:"audio_info"`
		Result struct {
			Text       string `json:"text"`
			Utterances []struct {
				Start     int    `json:"start_time"`
				End       int    `json:"end_time"`
				Text      string `json:"text"`
				Additions struct {
					Speaker string `json:"speaker"`
					Emotion string `json:"emotion"`
				} `json:"additions"`
			} `json:"utterances"`
		} `json:"result"`
	}
	if len(data) > maxRecordingResponseBytes || !utf8.Valid(data) || json.Unmarshal(data, &wire) != nil {
		return RecordingAnalysis{}, ErrInvalid
	}
	if wire.Audio.Duration <= 0 || wire.Audio.Duration > milliseconds+100 || wire.Audio.Duration < milliseconds-100 || len(wire.Result.Utterances) > 10000 || utf8.RuneCountInString(wire.Result.Text) > MaxContextCharacters {
		return RecordingAnalysis{}, ErrInvalid
	}
	result := RecordingAnalysis{TaskID: taskID, Version: RecordingAnalysisVersion, Milliseconds: wire.Audio.Duration, Text: wire.Result.Text, Utterances: []RecordingUtterance{}}
	previousStart := -1
	characters := 0
	for i, u := range wire.Result.Utterances {
		characters += utf8.RuneCountInString(u.Text)
		if u.Start < 0 || u.Start < previousStart || u.End <= u.Start || u.End > milliseconds || len(u.Additions.Speaker) > 128 || utf8.RuneCountInString(u.Additions.Emotion) > 256 || characters > MaxContextCharacters || strings.TrimSpace(u.Text) == "" {
			return RecordingAnalysis{}, ErrInvalid
		}
		previousStart = u.Start
		// A missing speaker stays unknown. Never manufacture a person or emotion.
		result.Utterances = append(result.Utterances, RecordingUtterance{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s:%d", taskID, i))).String(), Speaker: u.Additions.Speaker, StartMilliseconds: u.Start, EndMilliseconds: u.End, Text: u.Text, AcousticEmotion: u.Additions.Emotion})
	}
	if strings.TrimSpace(result.Text) != "" && len(result.Utterances) == 0 {
		return RecordingAnalysis{}, ErrInvalid
	}
	return result, nil
}

// Canonical WAV is created by the app from PCM chunks. Reject a mismatched
// header before provider submission so declared duration cannot bypass limits.
func validRecordingWAV(wav []byte) bool {
	if len(wav) < 46 || len(wav) > SessionMilliseconds*32+44 || (len(wav)-44)%2 != 0 {
		return false
	}
	u16 := binary.LittleEndian.Uint16
	u32 := binary.LittleEndian.Uint32
	return string(wav[:4]) == "RIFF" && u32(wav[4:8]) == uint32(len(wav)-8) && string(wav[8:16]) == "WAVEfmt " && u32(wav[16:20]) == 16 && u16(wav[20:22]) == 1 && u16(wav[22:24]) == 1 && u32(wav[24:28]) == 16000 && u32(wav[28:32]) == 32000 && u16(wav[32:34]) == 2 && u16(wav[34:36]) == 16 && string(wav[36:40]) == "data" && u32(wav[40:44]) == uint32(len(wav)-44)
}
