package albums

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

var ErrCOSOperation = errors.New("album object storage operation failed")

// COSConfig credentials are supplied by the server environment, never by clients.
// The bucket must be private with versioning enabled. Clients receive only signed
// staging PUTs; the server role alone can write/read original archive versions.
type COSConfig struct {
	Bucket       string
	Region       string
	SecretID     string
	SecretKey    string
	SessionToken string
	// Upload signer must have only PutObject on albums/staging/*, without
	// GetObject, ACL changes, bucket administration or archive access.
	UploadSecretID     string
	UploadSecretKey    string
	UploadSessionToken string
}

type COSObjects struct {
	client       *cos.Client
	uploadClient *cos.Client
	host         string
	now          func() time.Time
}

var cosBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}-[0-9]{5,20}$`)
var cosRegionPattern = regexp.MustCompile(`^[a-z]{2}-[a-z0-9-]{2,40}$`)

func NewCOSObjects(config COSConfig) (*COSObjects, error) {
	return newCOSObjects(config, http.DefaultTransport)
}

func newCOSObjects(config COSConfig, transport http.RoundTripper) (*COSObjects, error) {
	if !cosBucketPattern.MatchString(config.Bucket) || !cosRegionPattern.MatchString(config.Region) || config.SecretID == "" || config.SecretKey == "" || config.UploadSecretID == "" || config.UploadSecretKey == "" || config.UploadSecretID == config.SecretID || transport == nil {
		return nil, errors.New("invalid album COS configuration")
	}
	host := config.Bucket + ".cos." + config.Region + ".myqcloud.com"
	client := cos.NewClient(&cos.BaseURL{BucketURL: &url.URL{Scheme: "https", Host: host}}, &http.Client{
		Transport:     &cos.AuthorizationTransport{SecretID: config.SecretID, SecretKey: config.SecretKey, SessionToken: config.SessionToken, Transport: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	})
	uploadClient := cos.NewClient(&cos.BaseURL{BucketURL: &url.URL{Scheme: "https", Host: host}}, &http.Client{
		Transport:     &cos.AuthorizationTransport{SecretID: config.UploadSecretID, SecretKey: config.UploadSecretKey, SessionToken: config.UploadSessionToken, Transport: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	})
	return &COSObjects{client: client, uploadClient: uploadClient, host: host, now: time.Now}, nil
}

// Check verifies configuration without mutating the customer's bucket settings.
func (s *COSObjects) Check(ctx context.Context) error {
	result, _, err := s.client.Bucket.GetVersioning(ctx)
	if err != nil {
		return ErrCOSOperation
	}
	if result == nil || result.Status != "Enabled" {
		return ErrIncompleteBackup
	}
	return nil
}

func albumObjectKey(key, area string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 5 && parts[0] == "albums" && parts[1] == area && validOwner(parts[2]) && validOwner(parts[3]) && resourceIDPattern.MatchString(parts[4])
}

func (s *COSObjects) AuthorizePut(ctx context.Context, key string, resource Resource, expiry time.Time) (PutGrant, error) {
	duration := expiry.Sub(s.now())
	if !albumObjectKey(key, "staging") || !strings.HasSuffix(key, "/"+resource.ID) || resource.SizeBytes <= 0 || duration <= 0 || duration > 10*time.Minute {
		return PutGrant{}, ErrInvalidManifest
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/octet-stream")
	headers.Set("Content-Length", strconv.FormatInt(resource.SizeBytes, 10))
	signed, err := s.uploadClient.Object.GetPresignedURL2(ctx, http.MethodPut, key, duration, &cos.PresignedURLOptions{Header: &headers})
	if err != nil {
		return PutGrant{}, ErrCOSOperation
	}
	return PutGrant{ResourceID: resource.ID, URL: signed.String(), Headers: map[string]string{
		"Content-Type": "application/octet-stream", "Content-Length": strconv.FormatInt(resource.SizeBytes, 10),
	}, ExpiresAt: expiry}, nil
}

func concreteCOSVersion(value string) bool {
	return value != "" && value != "null" && !strings.ContainsAny(value, "\r\n")
}

func (s *COSObjects) Seal(ctx context.Context, from, to string) (string, error) {
	if !albumObjectKey(from, "staging") || !albumObjectKey(to, "originals") || strings.TrimPrefix(from, "albums/staging/") != strings.TrimPrefix(to, "albums/originals/") {
		return "", ErrInvalidManifest
	}
	source, err := s.client.Object.Head(ctx, from, nil)
	if err != nil {
		return "", ErrCOSOperation
	}
	version := source.Header.Get("x-cos-version-id")
	if !concreteCOSVersion(version) {
		return "", ErrIncompleteBackup
	}
	// MultiCopy fixes the source version for both its HEAD and every copied part.
	// Objects larger than 5 GB use bounded parallel multipart server-side copies.
	_, response, err := s.client.Object.MultiCopy(ctx, to, s.host+"/"+from, &cos.MultiCopyOptions{PartSize: 64, ThreadPoolSize: 2}, version)
	if err != nil {
		return "", ErrCOSOperation
	}
	if response == nil {
		return "", ErrIncompleteBackup
	}
	archived := response.Header.Get("x-cos-version-id")
	if !concreteCOSVersion(archived) {
		return "", ErrIncompleteBackup
	}
	return archived, nil
}

func (s *COSObjects) OpenVersion(ctx context.Context, key, version string) (io.ReadCloser, error) {
	if !albumObjectKey(key, "originals") || !concreteCOSVersion(version) {
		return nil, ErrIncompleteBackup
	}
	response, err := s.client.Object.Get(ctx, key, nil, version)
	if err != nil {
		return nil, ErrCOSOperation
	}
	if response == nil || response.Body == nil {
		return nil, ErrIncompleteBackup
	}
	if response.Header.Get("x-cos-version-id") != version {
		response.Body.Close()
		return nil, ErrIncompleteBackup
	}
	return response.Body, nil
}
