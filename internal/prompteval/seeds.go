package prompteval

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"strings"
	"unicode/utf8"
)

//go:embed fixtures/river.pcm
var riverAudio []byte

func Builtins() []Sample {
	hash := sha256.Sum256([]byte("synthetic journal sample"))
	request := contracts.OrganizeRequest{RequestID: "20000000-0000-4000-8000-000000000001", ContractVersion: contracts.ContractVersion, ContentHash: hex.EncodeToString(hash[:]), Title: "周末散步", Body: "周末下午，我去河边散步，途中买了一杯热茶。", ExistingTags: []string{}, RejectedTagNames: []string{}, Books: []contracts.BookContext{}}
	lite := request
	pro := request
	out := []Sample{{ID: "10000000-0000-4000-8000-000000000001", Name: "智能整理 · Lite", Kind: "organize_lite", Organize: &lite, Expected: []Check{}}, {ID: "10000000-0000-4000-8000-000000000002", Name: "智能整理 · Pro", Kind: "organize_pro", Organize: &pro, Expected: []Check{}}}
	for i, style := range []voice.WritingStyle{voice.StyleNatural, voice.StyleLively, voice.StyleDocumentary, voice.StyleDaybook, voice.StyleEssay} {
		id := "10000000-0000-4000-8000-00000000000" + string(rune('3'+i))
		blockID := "30000000-0000-4000-8000-000000000001"
		mediaID := "30000000-0000-4000-8000-000000000002"
		snapshot := voice.Snapshot{WritingStyle: style, Revision: 1, Blocks: []voice.Block{{ID: blockID, Text: "下午，我和小林在河边散步。"}, {ID: mediaID, Text: ""}}, Transcript: "下午我和小明在河边散步。", EditedBlockIDs: []string{blockID}, MediaOnlyBlockIDs: []string{mediaID}, ManualEdits: []voice.ManualEdit{{BlockID: blockID, Before: "小明", After: "小林", TranscriptOffset: utf8.RuneCountInString("下午我和小明在河边散步。"), PendingEarlierSpeech: true}}, Words: []string{}}
		out = append(out, Sample{ID: id, Name: "语音整理 · " + string(style), Kind: "voice", Voice: &snapshot, Expected: []Check{{Kind: "contains", BlockID: blockID, Text: "小林"}, {Kind: "excludes", BlockID: blockID, Text: "小明"}, {Kind: "unchanged", BlockID: mediaID}}})
	}

	snapshot := voice.Snapshot{WritingStyle: voice.StyleNatural, Revision: 1, Blocks: []voice.Block{{ID: "30000000-0000-4000-8000-000000000003", Text: ""}}, Transcript: "", EditedBlockIDs: []string{}, MediaOnlyBlockIDs: []string{}, ManualEdits: []voice.ManualEdit{}, Words: []string{"小林"}}
	out = append(out, Sample{ID: "10000000-0000-4000-8000-000000000008", Name: "语音识别与整理 · 合成音频", Kind: "voice", Voice: &snapshot, Audio: append([]byte{}, riverAudio...), Expected: []Check{{Kind: "contains", Text: "河边"}, {Kind: "contains", Text: "热茶"}}})
	return append(out, RequiredQualitySamples()...)
}

// These immutable synthetic cases are mandatory for publication; edited copies do not satisfy coverage.
func RequiredQualitySamples() []Sample {
	bookID := "00000000-0000-4000-8000-000000000001"
	reflection := "连续三周，我都在周末复盘自己的工作与生活。今天回顾了任务安排，发现我经常把简单的事拖到最后。我想以后每周都留一段时间思考自己的选择，并记录下一次可以怎样改进。"
	travel := "我和女朋友小林约好，以后每个月一起去一个地方旅行。这次去了桂林的阳朔，在漓江边骑行。以后还想一起去别的城市，慢慢记下两个人出游的经历。"
	cases := []struct {
		name, body         string
		books              []contracts.BookContext
		existing, rejected []string
		checks             []Check
	}{
		{name: "短文不凑标签", body: "今天吃了碗面，挺好吃。", checks: []Check{{Kind: "tag_limit", Text: "1"}, {Kind: "new_book_count", Text: "0"}}},
		{name: "已收录不重复建册", body: travel, books: []contracts.BookContext{{ID: bookID, Name: "情侣出游手记", Description: "我们两个人的出游经历。", ContainsEntry: true}}, existing: []string{"旅行"}, rejected: []string{"骑行"}, checks: []Check{{Kind: "new_book_count", Text: "0"}, {Kind: "existing_book_count", Text: "0"}}},
		{name: "保留用户书册名称", body: reflection, books: []contracts.BookContext{{ID: bookID, Name: "日常思考复盘手记", Description: "长期收录工作、生活中的思考与复盘。"}}, checks: []Check{{Kind: "new_book_count", Text: "0"}, {Kind: "existing_book_count", Text: "1"}}},
		{name: "不猜测人物关系", body: "今天和小林去河边散步，我们约好以后每周都去走一走。", checks: []Check{{Kind: "excludes", Text: "情侣"}, {Kind: "excludes", Text: "夫妻"}, {Kind: "excludes", Text: "亲子"}}},
		{name: "新书册名称简洁", body: reflection, checks: []Check{{Kind: "new_book_count", Text: "1"}}},
	}
	out := []Sample{}
	for _, kind := range []string{"organize_lite", "organize_pro"} {
		for _, c := range cases {
			id := fmt.Sprintf("10000000-0000-4000-8000-%012d", 100+len(out))
			request := contracts.OrganizeRequest{RequestID: id, ContractVersion: contracts.ContractVersion, ContentHash: strings.Repeat("0", 64), Body: c.body, ExistingTags: append([]string{}, c.existing...), RejectedTagNames: append([]string{}, c.rejected...), Books: append([]contracts.BookContext{}, c.books...)}
			out = append(out, Sample{ID: id, Name: c.name + " · " + kind, Kind: kind, Organize: &request, Expected: c.checks})
		}
	}
	scene := contracts.OrganizeRequest{RequestID: "10000000-0000-4000-8000-000000000110", ContractVersion: contracts.ContractVersion, ContentHash: strings.Repeat("0", 64), Body: "傍晚，小狗沿着河岸的柳树旁散步，一只蝴蝶停在草地上。", ExistingTags: []string{}, RejectedTagNames: []string{}, Books: []contracts.BookContext{}}
	out = append(out, Sample{ID: scene.RequestID, Name: "配图场景忠于原文", Kind: "illustration_scene", Organize: &scene, Expected: []Check{{Kind: "contains", Text: "小狗"}}})
	brief := illustration.Brief{Title: "河岸边的小狗", Details: []illustration.Detail{{Kind: "subject", Text: "小狗"}, {Kind: "setting", Text: "傍晚的河岸和柳树"}}}
	out = append(out, Sample{ID: "10000000-0000-4000-8000-000000000111", Name: "图片生成与格式验证", Kind: "illustration_image", Illustration: &brief, Expected: []Check{}})
	return out
}
