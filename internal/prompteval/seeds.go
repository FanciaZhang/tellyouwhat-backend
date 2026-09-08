package prompteval

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journal/voice"
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
	return out
}
