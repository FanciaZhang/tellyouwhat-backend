package provider

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"golang.org/x/text/unicode/norm"
)

const OrganizationAnalysisVersion = "journal-organize-2026-10-12"

const maxOrganizationTags = 5

// The wire contract stays compatible with existing Apps; generation is stricter.
func organizationSchema() map[string]any {
	schema := contracts.ResponseSchema()
	properties := schema["properties"].(map[string]any)
	properties["tags"].(map[string]any)["maxItems"] = maxOrganizationTags
	suggestions := properties["newBookSuggestions"].(map[string]any)
	suggestions["maxItems"] = 1
	suggestions["items"].(map[string]any)["properties"].(map[string]any)["name"].(map[string]any)["maxLength"] = 24
	return schema
}

var emptyOrganizationTags = map[string]bool{
	"生活": true, "日常": true, "记录": true, "手记": true, "手記": true,
	"日记": true, "日記": true, "感悟": true, "心情": true, "其他": true,
}

var unnamedPeople = map[string]bool{
	"我": true, "你": true, "他": true, "她": true, "我们": true, "他们": true,
	"爸爸": true, "妈妈": true, "老婆": true, "老公": true, "孩子": true,
	"宝宝": true, "儿子": true, "女儿": true, "父亲": true, "母亲": true,
	"丈夫": true, "妻子": true, "男朋友": true, "女朋友": true, "同事": true, "朋友": true,
}

func qualityName(value string) string {
	// Match the App's case-, width- and diacritic-insensitive name comparison.
	plain := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, norm.NFKD.String(value))
	return strings.ToLower(strings.Join(strings.Fields(norm.NFKC.String(plain)), " "))
}

// Only unambiguous recurring variants belong here; leave broader semantic
// judgments to the Prompt instead of merging unrelated topics by substring.
func organizationTagFamily(value string) string {
	switch value {
	case "旅行", "旅游", "出游", "情侣出游", "情侣旅行", "两人出游", "两人旅行":
		return "旅行"
	case "复盘", "每周复盘", "周末复盘", "日常复盘", "工作复盘", "自我复盘":
		return "复盘"
	}
	return ""
}

// Apply only to generated results, never to the user's saved names. Structural
// validation runs first, so this cannot hide malformed responses or unknown IDs.
func curateOrganization(request contracts.OrganizeRequest, result contracts.ModelResult) contracts.ModelResult {
	curated := contracts.ModelResult{
		Tags: []contracts.Tag{}, ExistingBookRecommendations: []contracts.ExistingBookRecommendation{},
		NewBookSuggestions:     []contracts.NewBookSuggestion{},
		IllustrationSuggestion: result.IllustrationSuggestion,
	}
	seenTags, rejected, related := map[string]bool{}, map[string]bool{}, map[string]string{}
	seenFamilies, rejectedFamilies := map[string]bool{}, map[string]bool{}
	for _, name := range request.RejectedTagNames {
		rejected[qualityName(name)] = true
		if family := organizationTagFamily(qualityName(name)); family != "" {
			rejectedFamilies[family] = true
		}
	}
	for _, name := range request.ExistingTags {
		key := qualityName(name)
		seenTags[key] = true
		if family := organizationTagFamily(key); family != "" {
			seenFamilies[family] = true
		}
		if !rejected[key] && !emptyOrganizationTags[key] {
			related[key] = name
		}
	}
	source := qualityName(request.Title + "\n" + request.Body)
	for _, tag := range result.Tags {
		key := qualityName(tag.Name)
		family := ""
		if tag.Type == "topic" || tag.Type == "event" || tag.Type == "other" {
			family = organizationTagFamily(key)
		}
		if seenTags[key] || rejected[key] || emptyOrganizationTags[key] || strings.HasPrefix(key, "#") {
			continue
		}
		if family != "" && (seenFamilies[family] || rejectedFamilies[family]) {
			continue
		}
		if tag.Type == "person" && unnamedPeople[key] {
			continue
		}
		if (tag.Type == "person" || tag.Type == "place" || tag.Type == "organization") && !strings.Contains(source, key) {
			continue
		}
		if len(curated.Tags) == maxOrganizationTags {
			break
		}
		seenTags[key] = true
		if family != "" {
			seenFamilies[family] = true
		}
		related[key] = tag.Name
		curated.Tags = append(curated.Tags, tag)
	}
	books, seenBooks := map[string]contracts.BookContext{}, map[string]bool{}
	for _, book := range request.Books {
		books[book.ID] = book
	}
	for _, rec := range result.ExistingBookRecommendations {
		book, exists := books[rec.BookID]
		if exists && !book.ContainsEntry {
			curated.ExistingBookRecommendations = append(curated.ExistingBookRecommendations, rec)
			seenBooks[rec.BookID] = true
		}
	}
	for _, suggestion := range result.NewBookSuggestions {
		name := conciseBookName(suggestion.Name)
		if name == "" {
			continue
		}
		// A suffix-free proposal may already exist under a user's original name.
		// Match exact names first; ambiguous suffix matches never create a duplicate.
		matches := []contracts.BookContext{}
		for _, book := range request.Books {
			if qualityName(book.Name) == qualityName(name) {
				matches = []contracts.BookContext{book}
				break
			}
			if cleaned := conciseBookName(book.Name); cleaned != "" && qualityName(cleaned) == qualityName(name) {
				matches = append(matches, book)
			}
		}
		if len(matches) > 0 {
			if len(matches) == 1 && !matches[0].ContainsEntry && !seenBooks[matches[0].ID] && len(curated.ExistingBookRecommendations) < 3 {
				curated.ExistingBookRecommendations = append(curated.ExistingBookRecommendations, contracts.ExistingBookRecommendation{BookID: matches[0].ID, Reason: suggestion.Reason})
				seenBooks[matches[0].ID] = true
			}
			continue
		}
		if len(curated.NewBookSuggestions) == 1 {
			continue
		}
		suggestion.Name = name
		originalRelated := suggestion.RelatedTags
		suggestion.RelatedTags = []string{}
		seenRelated := map[string]bool{}
		for _, value := range originalRelated {
			key := qualityName(value)
			if canonical, exists := related[key]; exists && !seenRelated[key] {
				suggestion.RelatedTags = append(suggestion.RelatedTags, canonical)
				seenRelated[key] = true
			}
		}
		curated.NewBookSuggestions = append(curated.NewBookSuggestions, suggestion)
	}
	return curated
}

func conciseBookName(value string) string {
	name := strings.TrimSpace(value)
	for _, suffix := range []string{"手记册", "手記冊", "手记", "手記", "记录", "記錄", "合集"} {
		name = strings.TrimSuffix(name, suffix)
	}
	name = strings.Trim(name, " \t\n·•—-:：")
	if name == "" || utf8.RuneCountInString(name) > 24 || strings.Contains(name, "手记") || strings.Contains(name, "手記") {
		return ""
	}
	return name
}

// Evaluation checks raw and final outputs independently; sanitization is not a Prompt pass.
func OrganizationQualityIssues(request contracts.OrganizeRequest, result contracts.ModelResult) []string {
	issues := []string{}
	curated := curateOrganization(request, result)
	if len(curated.Tags) != len(result.Tags) {
		issues = append(issues, "标签包含重复、空泛、被拒绝或原文未出现的实体")
	}
	if len(curated.ExistingBookRecommendations) != len(result.ExistingBookRecommendations) {
		issues = append(issues, "重复推荐已收录书册")
	}
	if len(curated.NewBookSuggestions) != len(result.NewBookSuggestions) {
		issues = append(issues, "新书册与已有书册重复")
	}
	for i, book := range result.NewBookSuggestions {
		if i < len(curated.NewBookSuggestions) && book.Name != curated.NewBookSuggestions[i].Name {
			issues = append(issues, "新书册名称包含冗余后缀")
		}
	}
	return issues
}
