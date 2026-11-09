// Package cloudbilling reads account-level supplier bills without payment actions.
package cloudbilling

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/volcengine/volcengine-go-sdk/service/billing"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
	"github.com/volcengine/volcengine-go-sdk/volcengine/request"
	"github.com/volcengine/volcengine-go-sdk/volcengine/session"
)

var ErrResponse = errors.New("incomplete supplier bill response")
var decimal = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]{1,9})?$`)

type API interface {
	ListBillOverviewByProdWithContext(volcengine.Context, *billing.ListBillOverviewByProdInput, ...request.Option) (*billing.ListBillOverviewByProdOutput, error)
}
type Client struct{ API API }
type Product struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Currency     string `json:"currency"`
	PayableNanos int64  `json:"payableNanos"`
	PaidNanos    int64  `json:"paidNanos"`
}

func NewFromFile(path string) (*Client, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("billing credential unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("invalid billing credential permissions")
	}
	var key struct {
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
	}
	d := json.NewDecoder(io.LimitReader(f, 8193))
	d.DisallowUnknownFields()
	if d.Decode(&key) != nil || key.AccessKey == "" || key.SecretKey == "" || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid billing credential")
	}
	s, err := session.NewSession(volcengine.NewConfig().WithRegion("cn-beijing").WithCredentials(credentials.NewStaticCredentials(key.AccessKey, key.SecretKey, "")).WithHTTPClient(&http.Client{Timeout: 15 * time.Second}).WithMaxRetries(0))
	if err != nil {
		return nil, errors.New("billing session unavailable")
	}
	return &Client{API: billing.New(s)}, nil
}

func parseAmount(raw *string) (int64, error) {
	if raw == nil || !decimal.MatchString(*raw) {
		return 0, ErrResponse
	}
	r, ok := new(big.Rat).SetString(*raw)
	if !ok {
		return 0, ErrResponse
	}
	r.Mul(r, big.NewRat(1_000_000_000, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, ErrResponse
	}
	return r.Num().Int64(), nil
}

// Read returns only complete pagination. No customer names, account identifiers,
// resource identifiers, or raw supplier errors leave this boundary.
func (c *Client) Read(ctx context.Context, period string) ([]Product, error) {
	if _, err := time.Parse("2006-01", period); err != nil {
		return nil, ErrResponse
	}
	out := []Product{}
	total := int32(-1)
	for offset := int32(0); offset < 10000; {
		page, err := c.API.ListBillOverviewByProdWithContext(ctx, &billing.ListBillOverviewByProdInput{BillPeriod: volcengine.String(period), Limit: volcengine.Int32(100), Offset: volcengine.Int32(offset), NeedRecordNum: volcengine.Int32(1), IgnoreZero: volcengine.Int32(0)})
		if err != nil {
			return nil, err
		}
		if page == nil || page.Total == nil || *page.Total < 0 || *page.Total > 10000 || len(page.List) > 100 {
			return nil, ErrResponse
		}
		if total < 0 {
			total = *page.Total
		} else if total != *page.Total {
			return nil, ErrResponse
		}
		if page.Offset != nil && *page.Offset != offset {
			return nil, ErrResponse
		}
		for _, row := range page.List {
			if row == nil || row.Product == nil || *row.Product == "" || row.Currency == nil || *row.Currency != "CNY" || (row.BillPeriod != nil && *row.BillPeriod != period) {
				return nil, ErrResponse
			}
			payable, err := parseAmount(row.PayableAmount)
			if err != nil {
				return nil, err
			}
			paid, err := parseAmount(row.PaidAmount)
			if err != nil {
				return nil, err
			}
			out = append(out, Product{Code: *row.Product, Name: volcengine.StringValue(row.ProductZh), Currency: *row.Currency, PayableNanos: payable, PaidNanos: paid})
		}
		offset += int32(len(page.List))
		if offset == total {
			return out, nil
		}
		if offset > total || len(page.List) == 0 {
			return nil, ErrResponse
		}
	}
	return nil, ErrResponse
}
