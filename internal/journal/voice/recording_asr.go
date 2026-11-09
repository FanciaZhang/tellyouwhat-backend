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
	"os"
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
	Status      string
	LogID       string
	taskMissing bool
}

func (e *RecordingProviderError) Error() string { return "recording_provider_" + e.Status }

func (e *RecordingProviderError) Is(target error) bool {
	return target == ErrRecordingTaskMissing && e.taskMissing
}

var ErrRecordingTaskMissing = errors.New("recording_task_missing")
var ErrRecordingPending = errors.New("recording_analysis_pending")

// The request ID is generated and durably saved by the caller BEFORE submit.
// After a lost response, query this ID instead of creating a second billed task.
func (a RecordingASR) Submit(ctx context.Context, taskID string, wav []byte) error {
	if _, err := uuid.Parse(taskID); err != nil || !validRecordingWAV(wav) {
		return ErrInvalid
	}
	return a.submitAudio(ctx, taskID, bytes.NewReader(wav), int64(len(wav)))
}

// SubmitFile keeps a long recording on private temporary storage instead of
// allocating its whole PCM and base64 copies in the development service heap.
func (a RecordingASR) SubmitFile(ctx context.Context, taskID, path string) error {
	if _, err := uuid.Parse(taskID); err != nil {
		return ErrInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ErrInvalid
	}
	header := make([]byte, 44)
	if _, err = io.ReadFull(file, header); err != nil || recordingWAVDuration(header, info.Size()) <= 0 {
		return ErrInvalid
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return a.submitAudio(ctx, taskID, io.LimitReader(file, info.Size()), info.Size())
}
func (a RecordingASR) submitAudio(ctx context.Context, taskID string, audio io.Reader, audioLength int64) error {
	// Stream base64 into the HTTP request instead of holding both 58 MB PCM
	// and its 77 MB JSON copy in the private service's 192 MB memory limit.
	prefix := `{"user":{"uid":"journal-recording"},"audio":{"format":"wav","data":"`
	// Use the documented ASR 2.0 diarization version explicitly, as the live
	// stream does. Omitting this option does not establish which version ran.
	suffix := `"},"request":{"model_name":"bigmodel","enable_itn":true,"enable_punc":true,"show_utterances":true,"enable_speaker_info":true,"ssd_version":"200","enable_emotion_detection":true}}`
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		_, err := io.WriteString(writer, prefix)
		if err == nil {
			encoder := base64.NewEncoder(base64.StdEncoding, writer)
			_, err = io.Copy(encoder, audio)
			if closeErr := encoder.Close(); err == nil {
				err = closeErr
			}
		}
		if err == nil {
			_, err = io.WriteString(writer, suffix)
		}
		_ = writer.CloseWithError(err)
	}()
	length := int64(len(prefix) + base64.StdEncoding.EncodedLen(int(audioLength)) + len(suffix))
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
		// 45000000 alone is a generic client failure, not evidence of absence.
		// Match only the observed query diagnostic; do not retain arbitrary
		// provider messages (they may contain private request data).
		missing := action == "query" && response.StatusCode == http.StatusOK && status == "45000000" &&
			strings.TrimSpace(response.Header.Get("X-Api-Message")) == "[Client-side generic error] OperatorWrapper Process failed: cannot find task"
		return nil, &RecordingProviderError{Status: status, LogID: response.Header.Get("X-Tt-Logid"), taskMissing: missing}
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
func validRecordingWAV(wav []byte) bool { return RecordingWAVMilliseconds(wav) > 0 }

// Preserve one or two source channels for file analysis; streaming capture's
// mono format must not be imposed on an imported stereo recording.
func RecordingWAVMilliseconds(wav []byte) int {
	return recordingWAVDuration(wav, int64(len(wav)))
}
func recordingWAVDuration(wav []byte, size int64) int {
	if len(wav) < 44 || size < 76 || size > SessionMilliseconds*64+44 {
		return 0
	}
	u16 := binary.LittleEndian.Uint16
	u32 := binary.LittleEndian.Uint32
	channels := int(u16(wav[22:24]))
	if channels != 1 && channels != 2 {
		return 0
	}
	frameBytes := channels * 2
	if string(wav[:4]) != "RIFF" || int64(u32(wav[4:8])) != size-8 || string(wav[8:16]) != "WAVEfmt " || u32(wav[16:20]) != 16 || u16(wav[20:22]) != 1 || u32(wav[24:28]) != 16000 || u32(wav[28:32]) != uint32(16000*frameBytes) || u16(wav[32:34]) != uint16(frameBytes) || u16(wav[34:36]) != 16 || string(wav[36:40]) != "data" || int64(u32(wav[40:44])) != size-44 || (size-44)%int64(frameBytes) != 0 {
		return 0
	}
	milliseconds := int((size - 44) / int64(16*frameBytes))
	if milliseconds <= 0 || milliseconds > SessionMilliseconds {
		return 0
	}
	return milliseconds
}
