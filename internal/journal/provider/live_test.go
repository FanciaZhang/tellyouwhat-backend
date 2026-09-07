package provider

import (
	"context"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicit operator acceptance; only fixed synthetic text, never private journals.
func TestLiveJournalOrganization(t *testing.T) {
	if os.Getenv("JOURNAL_ORGANIZE_LIVE_CHECK") != "1" {
		t.Skip("explicit live provider acceptance only")
	}
	base := os.Getenv("JOURNAL_ARK_BASE_URL")
	if base == "" {
		base = "https://ark.cn-beijing.volces.com/api/v3"
	}
	client := New(Config{BaseURL: base, APIKey: os.Getenv("JOURNAL_ARK_API_KEY"), LiteModel: os.Getenv("JOURNAL_ARK_LITE_MODEL_ID"), ProModel: os.Getenv("JOURNAL_ARK_PRO_MODEL_ID")}, &http.Client{Timeout: 75 * time.Second})
	for _, pro := range []bool{false, true} {
		name := "lite"
		if pro {
			name = "pro"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			defer cancel()
			result, err := client.Organize(ctx, contracts.OrganizeRequest{RequestID: uuid.NewString(), ContractVersion: contracts.ContractVersion, ContentHash: strings.Repeat("0", 64), Title: "公园散步", Body: "这是一条合成测试手记。今天下午和小林到公园散步，后来在湖边喝茶。", ExistingTags: []string{}, RejectedTagNames: []string{}, Books: []contracts.BookContext{}}, pro)
			if err != nil {
				t.Fatal(err)
			}
			if result.InputTokens <= 0 || result.OutputTokens <= 0 {
				t.Fatal("provider returned no billed result")
			}
			t.Logf("synthetic organization passed: tokens %d/%d", result.InputTokens, result.OutputTokens)
		})
	}
}
