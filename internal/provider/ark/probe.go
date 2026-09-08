package ark

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"

	"github.com/tellyouwhat/backend/internal/contracts"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
)

// Probe exercises the production serializer, streaming parser and schema
// validator using synthetic input. Its separate transport limits probe output;
// ordinary Health requests retain their existing generation semantics.
func (client *Client) Probe(ctx context.Context, model string, op contracts.Operation, policy contracts.ExecutionPolicy) error {
	if client.config.APIKey == "" {
		return errors.New("模型检查未配置服务 API Key")
	}
	request := contracts.Request{Operation: op, Prompt: `Return exactly {"ok":true}. This is a synthetic protocol check; no explanation is needed.`, ResponseSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean","enum":[true]}},"required":["ok"],"additionalProperties":false}`)}
	policy.Endpoint = model
	policy.TimeoutSeconds = 25
	request.ExecutionPolicy = &policy
	capability, _ := contracts.PolicyFor(op)
	if _, ok := capability.AllowedMedia["audio"]; ok {
		request.Media = []contracts.Media{{Kind: "audio"}}
	} else if _, ok := capability.AllowedMedia["image"]; ok {
		request.Media = []contracts.Media{{Kind: "image"}}
	}
	transport := client.http.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	bounded := New(client.config, &http.Client{Transport: probeTransport{transport}}, probeMedia{})
	// Both response modes are accepted by the Health request contract.
	if _, err := bounded.Complete(ctx, request); err != nil {
		return probeError(err)
	}
	if err := bounded.Stream(ctx, request, func(providerapi.StreamEvent) error { return nil }); err != nil {
		return probeError(err)
	}
	return nil
}
func probeError(err error) error {
	// The production client omits upstream response bodies. Avoid returning URL,
	// credentials, generated content or transport details through administration.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.New("模型检查超时，请稍后重试")
	}
	return errors.New("当前版本未通过 Responses、结构化输出或所选思考/媒体/联网参数检查；请调整参数或选择其他版本后重试")
}

type probeTransport struct{ http.RoundTripper }

func (t probeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	r.Body.Close()
	body["max_output_tokens"] = 2048
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return t.RoundTripper.RoundTrip(r)
}

type probeMedia struct{}

func (probeMedia) Resolve(_ context.Context, m contracts.Media) (string, error) {
	var raw []byte
	if m.Kind == "image" {
		var b bytes.Buffer
		if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
			return "", err
		}
		raw = b.Bytes()
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw), nil
	}
	raw = make([]byte, 44+32000)
	copy(raw, "RIFF")
	binary.LittleEndian.PutUint32(raw[4:], uint32(len(raw)-8))
	copy(raw[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(raw[16:], 16)
	binary.LittleEndian.PutUint16(raw[20:], 1)
	binary.LittleEndian.PutUint16(raw[22:], 1)
	binary.LittleEndian.PutUint32(raw[24:], 16000)
	binary.LittleEndian.PutUint32(raw[28:], 32000)
	binary.LittleEndian.PutUint16(raw[32:], 2)
	binary.LittleEndian.PutUint16(raw[34:], 16)
	copy(raw[36:], "data")
	binary.LittleEndian.PutUint32(raw[40:], 32000)
	return "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(raw), nil
}
