package appstoreconnect

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrReportNotAvailable = errors.New("Apple report not available")

func (client *Client) ReportsConfigured() bool {
	return client.config.AppAppleID != "" && client.config.VendorNumber != ""
}

// DownloadOfferReport uses Apple's daily aggregate report, never user identity or
// a claim-page acknowledgement, to establish custom-code redemption counts.
func (client *Client) DownloadOfferReport(ctx context.Context, day string) ([]byte, error) {
	if !client.ReportsConfigured() {
		return nil, ErrUnavailable
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return nil, ErrInvalid
	}
	path := "/v1/salesReports"
	query := url.Values{"filter[frequency]": {"DAILY"}, "filter[reportDate]": {day}, "filter[reportSubType]": {"SUMMARY"}, "filter[reportType]": {"SUBSCRIPTION_OFFER_CODE_REDEMPTION"}, "filter[vendorNumber]": {client.config.VendorNumber}, "filter[version]": {"1_0"}}
	token, err := client.bearerToken([]string{"GET " + path})
	if err != nil {
		return nil, ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(client.config.BaseURL, "/")+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/a-gzip")
	response, err := client.config.HTTPClient.Do(request)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrReportNotAvailable
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrForbidden
	default:
		return nil, ErrUnavailable
	}
	reader, err := gzip.NewReader(io.LimitReader(response.Body, maximumCSVBytes+1))
	if err != nil {
		return nil, ErrInvalid
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, maximumCSVBytes+1))
	if err != nil || len(raw) > maximumCSVBytes {
		return nil, ErrInvalid
	}
	return raw, nil
}
