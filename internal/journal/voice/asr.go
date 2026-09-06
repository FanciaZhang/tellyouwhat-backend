package voice

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

type ASRConfig struct{ URL, APIKey, AppKey, AccessKey, ResourceID string }
type Transcript struct {
	Text, Stable string
	Final        bool
}
type SpeechConnection interface {
	Send([]byte, bool) error
	Receive() (Transcript, error)
	Close() error
}
type Speech interface {
	Open(context.Context, []string) (SpeechConnection, error)
}
type ASR struct{ Config ASRConfig }
type asrConnection struct{ ws *websocket.Conn }

// Protocol source: https://www.volcengine.com/docs/6561/1354869
func (a ASR) Open(ctx context.Context, words []string) (SpeechConnection, error) {
	config, err := websocket.NewConfig(a.Config.URL, "https://api.journal.tellyouwhat.cn")
	if err != nil {
		return nil, err
	}
	config.Dialer = &net.Dialer{Timeout: 10 * time.Second}
	if a.Config.APIKey != "" {
		config.Header.Set("X-Api-Key", a.Config.APIKey)
	} else {
		config.Header.Set("X-Api-App-Key", a.Config.AppKey)
		config.Header.Set("X-Api-Access-Key", a.Config.AccessKey)
	}
	config.Header.Set("X-Api-Resource-Id", a.Config.ResourceID)
	config.Header.Set("X-Api-Request-Id", uuid.NewString())
	ws, err := config.DialContext(ctx)
	if err != nil {
		return nil, errors.New("speech_connection_failed")
	}
	ws.MaxPayloadBytes = 2 << 20
	hotwords := []map[string]string{}
	for _, w := range words {
		hotwords = append(hotwords, map[string]string{"word": w})
	}
	corpus, _ := json.Marshal(map[string]any{"hotwords": hotwords})
	payload, _ := json.Marshal(map[string]any{
		"user":    map[string]string{"uid": uuid.NewString()},
		"audio":   map[string]any{"format": "pcm", "codec": "raw", "rate": 16000, "bits": 16, "channel": 1},
		"request": map[string]any{"model_name": "bigmodel", "enable_nonstream": true, "show_utterances": true, "result_type": "full", "enable_itn": true, "enable_punc": true, "corpus": map[string]string{"context": string(corpus)}},
	})
	if err = websocket.Message.Send(ws, asrPacket(1, false, payload)); err != nil {
		ws.Close()
		return nil, err
	}
	return &asrConnection{ws}, nil
}
func asrPacket(kind byte, final bool, payload []byte) []byte {
	flags := byte(0)
	if final {
		flags = 2
	}
	serialization := byte(0)
	if kind == 1 {
		serialization = 0x10
	}
	packet := make([]byte, 8+len(payload))
	copy(packet, []byte{0x11, kind<<4 | flags, serialization, 0})
	binary.BigEndian.PutUint32(packet[4:8], uint32(len(payload)))
	copy(packet[8:], payload)
	return packet
}
func (c *asrConnection) Send(pcm []byte, final bool) error {
	c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return websocket.Message.Send(c.ws, asrPacket(2, final, pcm))
}
func (c *asrConnection) Close() error { return c.ws.Close() }
func (c *asrConnection) Receive() (Transcript, error) {
	c.ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var packet []byte
	if err := websocket.Message.Receive(c.ws, &packet); err != nil {
		return Transcript{}, err
	}
	return parseASR(packet)
}
func parseASR(packet []byte) (Transcript, error) {
	if len(packet) < 8 || packet[0]>>4 != 1 {
		return Transcript{}, ErrInvalid
	}
	offset := int(packet[0]&15) * 4
	if offset < 4 || offset+4 > len(packet) {
		return Transcript{}, ErrInvalid
	}
	kind := packet[1] >> 4
	flags := packet[1] & 15
	if kind == 15 {
		return Transcript{}, errors.New("speech_provider_error")
	}
	if kind != 9 {
		return Transcript{}, ErrInvalid
	}
	if flags&1 != 0 {
		offset += 4
	}
	if offset+4 > len(packet) {
		return Transcript{}, ErrInvalid
	}
	size := int(binary.BigEndian.Uint32(packet[offset : offset+4]))
	offset += 4
	if size > 2<<20 || size != len(packet)-offset {
		return Transcript{}, ErrInvalid
	}
	payload := packet[offset:]
	if packet[2]&15 == 1 {
		r, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return Transcript{}, err
		}
		defer r.Close()
		payload, err = io.ReadAll(io.LimitReader(r, (2<<20)+1))
		if err != nil || len(payload) > 2<<20 {
			return Transcript{}, ErrInvalid
		}
	} else if packet[2]&15 != 0 {
		return Transcript{}, ErrInvalid
	}
	var envelope struct {
		Result struct {
			Text       string `json:"text"`
			Utterances []struct {
				Text     string `json:"text"`
				Definite bool   `json:"definite"`
			} `json:"utterances"`
		} `json:"result"`
		Code int `json:"code"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return Transcript{}, fmt.Errorf("speech_invalid_response: %w", err)
	}
	if envelope.Code != 0 && envelope.Code != 20000000 {
		return Transcript{}, errors.New("speech_provider_error")
	}
	result := Transcript{Text: envelope.Result.Text, Final: flags&2 != 0}
	for _, u := range envelope.Result.Utterances {
		if u.Definite {
			result.Stable += u.Text
		}
	}
	if result.Final {
		result.Stable = result.Text
	}
	return result, nil
}
