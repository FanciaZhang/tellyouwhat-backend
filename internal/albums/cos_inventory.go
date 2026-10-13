package albums

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

// Decode the documented XML fields directly: the installed SDK misspells
// multipart CommonPrefixes and Delimiter, which must not hide grouped results.
type cosVersionInventory struct {
	XMLName                                                                      xml.Name `xml:"ListVersionsResult"`
	Name, Prefix, KeyMarker, VersionIdMarker, NextKeyMarker, NextVersionIdMarker string
	EncodingType, Delimiter                                                      string
	MaxKeys                                                                      int
	IsTruncated                                                                  *bool
	CommonPrefixes                                                               []string `xml:"CommonPrefixes>Prefix"`
	Version                                                                      []cos.ListVersionsResultVersion
	DeleteMarker                                                                 []cos.ListVersionsResultDeleteMarker
}

type cosMultipartInventory struct {
	XMLName                                             xml.Name `xml:"ListMultipartUploadsResult"`
	Bucket, Prefix, KeyMarker, NextKeyMarker, Delimiter string
	EncodingType                                        string `xml:"Encoding-Type"`
	UploadIDMarker                                      string `xml:"UploadIdMarker"`
	NextUploadIDMarker                                  string `xml:"NextUploadIdMarker"`
	MaxUploads                                          int
	IsTruncated                                         *bool
	CommonPrefixes                                      []string `xml:"CommonPrefixes>Prefix"`
	Uploads                                             []struct {
		Key      string
		UploadID string `xml:"UploadId"`
	} `xml:"Upload"`
}

func (s *COSObjects) readCOSInventory(ctx context.Context, query url.Values, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u := url.URL{Scheme: "https", Host: s.host, Path: "/", RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ErrCOSOperation
	}
	response, err := s.archiveHTTP.Do(req)
	if err != nil {
		return ErrCOSOperation
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrCOSOperation
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 || ctx.Err() != nil {
		return ErrCOSOperation
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(result) != nil {
		return ErrCOSOperation
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return ErrCOSOperation
	}
	return nil
}

func (s *COSObjects) readMultipartInventory(ctx context.Context, prefix, keyMarker, idMarker string) (*cosMultipartInventory, error) {
	query := url.Values{"uploads": {""}, "prefix": {prefix}, "max-uploads": {"1000"}}
	if keyMarker != "" {
		query.Set("key-marker", keyMarker)
	}
	if idMarker != "" {
		query.Set("upload-id-marker", idMarker)
	}
	var result cosMultipartInventory
	if err := s.readCOSInventory(ctx, query, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
