package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/contracts"
	journalcontracts "github.com/tellyouwhat/backend/internal/journal/contracts"
	journalprovider "github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/provider/ark"
)

type checkResult struct {
	Check  string `json:"check"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

func main() {
	models := flag.Bool("models", false, "also call Health and Journal models with synthetic input")
	flag.Parse()
	cfg, err := config.LoadPlatform()
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(checkResult{Check: "configuration", Detail: err.Error()})
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 75 * time.Second}
	store, err := media.NewTOSStore(cfg.TOS)
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(checkResult{Check: "media", Detail: "cannot initialize object storage"})
		os.Exit(1)
	}
	failed := false
	report := func(name string, err error) {
		result := checkResult{Check: name, Passed: err == nil}
		if err != nil {
			failed = true
			result.Detail = err.Error()
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	}
	asset, err := checkMedia(ctx, client, store)
	report("tos_upload_read_private", err)
	if err == nil {
		if *models {
			for _, app := range cfg.Apps {
				switch app.Registry.ID {
				case appregistry.Health:
					provider := ark.New(app.Ark, client, store)
					for _, operation := range []contracts.Operation{contracts.OperationMealTextCapture, contracts.OperationMealPhotoCapture, contracts.OperationHydrationCupEstimate} {
						request := syntheticHealthRequest(operation, asset)
						_, err := provider.Complete(ctx, request)
						if err != nil {
							err = safeProviderError(err)
						}
						report("health_"+string(operation), err)
					}
				case appregistry.Journal:
					provider := journalprovider.New(journalprovider.Config{
						BaseURL: app.JournalAI.BaseURL, APIKey: app.JournalAI.APIKey,
						LiteModel: app.JournalAI.LiteModel, ProModel: app.JournalAI.ProModel,
					}, client)
					for _, pro := range []bool{false, true} {
						_, err := provider.Organize(ctx, journalcontracts.OrganizeRequest{
							RequestID: randomUUID(), ContractVersion: journalcontracts.ContractVersion,
							ContentHash: hex.EncodeToString(make([]byte, 32)),
							Title:       "服务验证", Body: "这是一条合成测试手记：今天在公园散步。",
							ExistingTags: []string{}, RejectedTagNames: []string{}, Books: []journalcontracts.BookContext{},
						}, pro)
						if err != nil {
							err = safeProviderError(err)
						}
						name := "journal_lite"
						if pro {
							name = "journal_pro"
						}
						report(name, err)
					}
				}
			}
		}
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = store.Delete(deleteCtx, asset)
		deleteCancel()
		if err != nil {
			err = errors.New("synthetic object deletion failed")
		}
		report("tos_delete", err)
	}
	if failed {
		os.Exit(1)
	}
}

func checkMedia(ctx context.Context, client *http.Client, store *media.TOSStore) (asset contracts.Media, resultErr error) {
	canvas := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			canvas.SetRGBA(x, y, color.RGBA{R: 128, G: 128, B: 128, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		return asset, errors.New("cannot create synthetic image")
	}
	digest := sha256.Sum256(encoded.Bytes())
	request := media.UploadRequest{
		RequestID: randomUUID(), Operation: contracts.OperationMealPhotoCapture,
		MediaID: "service-check", Kind: "image", MIMEType: "image/png",
		SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(encoded.Len()),
	}
	// Object scope contains only generated identifiers and is never a user device.
	service := media.NewService(store, media.NewMemoryRegistry(), time.Now)
	authorization, err := service.Authorize(ctx, attestation.Principal{
		AppID: "health", KeyID: "service-check", DeviceID: "service-check-" + randomUUID(),
	}, request)
	if err != nil {
		return asset, errors.New("cannot authorize synthetic media upload")
	}
	asset = contracts.Media{
		ID: request.MediaID, Kind: request.Kind, MIMEType: request.MIMEType,
		ObjectID: authorization.ObjectID, SHA256: request.SHA256, SizeBytes: request.SizeBytes,
	}
	defer func() {
		if resultErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := store.Delete(cleanupCtx, asset); err != nil {
				resultErr = fmt.Errorf("%w; synthetic object cleanup also failed", resultErr)
			}
		}
	}()
	put, _ := http.NewRequestWithContext(ctx, http.MethodPut, authorization.UploadURL, bytes.NewReader(encoded.Bytes()))
	for name, value := range authorization.RequiredHeaders {
		put.Header.Set(name, value)
	}
	response, err := client.Do(put)
	if err != nil {
		return asset, errors.New("synthetic media upload transport failed")
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode/100 != 2 {
		return asset, fmt.Errorf("synthetic media upload returned HTTP %d", response.StatusCode)
	}
	readURL, err := store.Resolve(ctx, asset)
	if err != nil {
		return asset, errors.New("cannot authorize synthetic media download")
	}
	get, _ := http.NewRequestWithContext(ctx, http.MethodGet, readURL, nil)
	response, err = client.Do(get)
	if err != nil {
		return asset, errors.New("synthetic media download transport failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.Equal(body, encoded.Bytes()) {
		return asset, fmt.Errorf("synthetic media round trip failed (HTTP %d)", response.StatusCode)
	}
	unsigned, err := url.Parse(readURL)
	if err != nil {
		return asset, errors.New("invalid media download URL")
	}
	unsigned.RawQuery = ""
	get, _ = http.NewRequestWithContext(ctx, http.MethodGet, unsigned.String(), nil)
	response, err = client.Do(get)
	if err != nil {
		return asset, errors.New("cannot verify anonymous object access")
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 403 {
		return asset, fmt.Errorf("anonymous object access must be denied (HTTP %d)", response.StatusCode)
	}
	return asset, nil
}

func syntheticHealthRequest(operation contracts.Operation, asset contracts.Media) contracts.Request {
	request := contracts.Request{
		RequestID: randomUUID(), Operation: operation, ContractVersion: contracts.ContractVersionV1,
		Prompt:         "这是自动化合成输入的服务检查。请仅返回 {\"ok\":true}。若有图片，它是一张合成灰色图片，不包含真实用户数据。",
		ResponseSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		Options:        contracts.RequestOptions{ReasoningEffort: "minimal"},
	}
	if operation != contracts.OperationMealTextCapture {
		request.Media = []contracts.Media{asset}
	}
	return request
}

func safeProviderError(err error) error {
	// Provider errors can include signed object URLs. Keep diagnostics credential-free.
	var status int
	if _, scanErr := fmt.Sscanf(err.Error(), "ark status %d", &status); scanErr == nil {
		return fmt.Errorf("provider returned HTTP %d", status)
	}
	if _, scanErr := fmt.Sscanf(err.Error(), "provider status %d", &status); scanErr == nil {
		return fmt.Errorf("provider returned HTTP %d", status)
	}
	return errors.New("provider transport or structured response validation failed")
}

func randomUUID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("secure randomness unavailable")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:])
}
