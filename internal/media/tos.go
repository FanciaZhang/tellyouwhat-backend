package media

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos/enum"
)

type TOSConfig struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
}

type tosURLClient interface {
	PreSignedURL(*tos.PreSignedURLInput) (*tos.PreSignedURLOutput, error)
	DeleteObjectV2(context.Context, *tos.DeleteObjectV2Input) (*tos.DeleteObjectV2Output, error)
}

type TOSStore struct {
	client tosURLClient
	bucket string
	now    func() time.Time
}

func NewTOSStore(config TOSConfig) (*TOSStore, error) {
	if config.Endpoint == "" || config.Region == "" || config.Bucket == "" || config.AccessKey == "" || config.SecretKey == "" {
		return nil, errors.New("incomplete TOS configuration")
	}
	client, err := tos.NewClientV2(
		config.Endpoint,
		tos.WithRegion(config.Region),
		tos.WithCredentials(tos.NewStaticCredentials(config.AccessKey, config.SecretKey)),
	)
	if err != nil {
		return nil, err
	}
	return &TOSStore{client: client, bucket: config.Bucket, now: time.Now}, nil
}

func (store *TOSStore) PresignPut(
	_ context.Context,
	objectID,
	mimeType,
	digest string,
	size int64,
	expiresAt time.Time,
) (string, error) {
	if store == nil || store.client == nil || !strings.HasPrefix(objectID, "ai-temp/") {
		return "", errors.New("invalid TOS object scope")
	}
	expires := int64(expiresAt.Sub(store.now()).Seconds())
	if expires < 1 {
		return "", errors.New("TOS authorization already expired")
	}
	output, err := store.client.PreSignedURL(&tos.PreSignedURLInput{
		HTTPMethod: enum.HttpMethodPut,
		Bucket:     store.bucket,
		Key:        objectID,
		Expires:    expires,
		Header: map[string]string{
			"Content-Type":         mimeType,
			"Content-Length":       strconv.FormatInt(size, 10),
			"x-tos-content-sha256": digest,
		},
	})
	if err != nil {
		return "", err
	}
	return output.SignedUrl, nil
}

func (store *TOSStore) Resolve(_ context.Context, value contracts.Media) (string, error) {
	if store == nil || store.client == nil || !strings.HasPrefix(value.ObjectID, "ai-temp/") {
		return "", errors.New("invalid managed media object")
	}
	output, err := store.client.PreSignedURL(&tos.PreSignedURLInput{
		HTTPMethod: enum.HttpMethodGet,
		Bucket:     store.bucket,
		Key:        value.ObjectID,
		Expires:    int64((15 * time.Minute).Seconds()),
	})
	if err != nil {
		return "", err
	}
	return output.SignedUrl, nil
}

func (store *TOSStore) Delete(ctx context.Context, value contracts.Media) error {
	if store == nil || store.client == nil || !strings.HasPrefix(value.ObjectID, "ai-temp/") {
		return errors.New("invalid managed media object")
	}
	_, err := store.client.DeleteObjectV2(ctx, &tos.DeleteObjectV2Input{
		Bucket: store.bucket,
		Key:    value.ObjectID,
	})
	return err
}

func (store *TOSStore) DeleteObject(ctx context.Context, objectID string) error {
	return store.Delete(ctx, contracts.Media{ObjectID: objectID})
}

var _ Presigner = (*TOSStore)(nil)
