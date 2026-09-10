package voice

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

const rewriteHistoryCharacters = 1200
const rewriteBodyCharacters = 6000
const rewriteFragmentCharacters = 1000

// Only Service supplies the acknowledged source, after the App accepts a
// revision. A transport client cannot claim that unprocessed speech was handled.
type editorialWindow struct {
	TranscriptStart       int                 `json:"transcriptStart"`
	OmittedBodyCharacters int                 `json:"omittedBodyCharacters"`
	Fragments             []editorialFragment `json:"fragments,omitempty"`
}
type editorialFragment struct {
	ID      string `json:"id"`
	BlockID string `json:"blockID"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
}

type sourceFragment struct {
	editorialFragment
	text  string
	score int
}

func sharedSpeechPrefix(a, b string) int {
	left, right := []rune(a), []rune(b)
	i := 0
	for i < len(left) && i < len(right) && left[i] == right[i] {
		i++
	}
	return i
}

// Keep all not-yet-acknowledged speech. Reconnection, first reconciliation and
// a large backlog use the validated full snapshot; they must never silently
// drop speech to meet a routine incremental target.
func incrementalTranscriptStart(s Snapshot) int {
	if s.RecordingContext != nil || s.rewriteAcknowledged == "" {
		return 0
	}
	prefix := sharedSpeechPrefix(s.rewriteAcknowledged, s.Transcript)
	if len([]rune(s.Transcript))-prefix > 4000 {
		return 0
	}
	return max(0, prefix-rewriteHistoryCharacters)
}

func editorialBigrams(text string) map[string]bool {
	chars := []rune(strings.ToLower(text))
	out := map[string]bool{}
	for i := 1; i < len(chars); i++ {
		if (unicode.IsLetter(chars[i-1]) || unicode.IsNumber(chars[i-1])) && (unicode.IsLetter(chars[i]) || unicode.IsNumber(chars[i])) {
			out[string(chars[i-1:i+1])] = true
		}
	}
	return out
}

// Retrieval searches only the current local snapshot; no remote history,
// embedding store or extra model call. These are verbatim fragments, never a
// generated summary that could quietly alter an older fact.
func boundedEditorialBlocks(s Snapshot) ([]Block, []editorialFragment, int) {
	total := 0
	for _, b := range s.Blocks {
		total += len([]rune(b.Text))
	}
	if s.RecordingContext != nil || s.rewriteAcknowledged == "" || total <= rewriteBodyCharacters || len([]rune(s.Transcript))-sharedSpeechPrefix(s.rewriteAcknowledged, s.Transcript) > 4000 {
		return s.Blocks, nil, 0
	}
	query := []rune(s.Transcript)
	prefix := sharedSpeechPrefix(s.rewriteAcknowledged, s.Transcript)
	if prefix < len(query) {
		query = query[prefix:]
	}
	query = query[max(0, len(query)-300):]
	terms := editorialBigrams(string(query))
	media := map[string]bool{}
	for _, id := range s.MediaOnlyBlockIDs {
		media[id] = true
	}
	var fragments []sourceFragment
	for _, b := range s.Blocks {
		if media[b.ID] {
			continue
		}
		chars := []rune(b.Text)
		for start := 0; start < len(chars) || start == 0; {
			end := min(len(chars), start+rewriteFragmentCharacters)
			if end < len(chars) {
				for end > start && (!strings.ContainsRune("。！？；.!?; \n\t", chars[end-1]) || unicode.IsMark(chars[end]) || chars[end] == 0x200d) {
					end--
				}
				// Do not split a word, emoji sequence or other unbroken text.
				// This unusual source uses the existing full-context safety cap.
				if end == start {
					return s.Blocks, nil, 0
				}
			}
			id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(b.ID+":"+strconv.Itoa(start))).String()
			text := string(chars[start:end])
			score := 0
			for term := range editorialBigrams(text) {
				if terms[term] {
					score++
				}
			}
			fragments = append(fragments, sourceFragment{editorialFragment{id, b.ID, start, end}, text, score})
			if end == len(chars) {
				break
			}
			start = end
		}
	}
	selected := map[int]bool{}
	used := 0
	add := func(i int) bool {
		n := fragments[i].End - fragments[i].Start
		if selected[i] {
			return true
		}
		if used+n > rewriteBodyCharacters || len(selected) >= 8 {
			return false
		}
		selected[i] = true
		used += n
		return true
	}
	// Current ending and opening orientation, then source-related older text.
	recent := 0
	for i := len(fragments) - 1; i >= 0 && recent < 3000; i-- {
		if add(i) {
			recent += fragments[i].End - fragments[i].Start
		}
	}
	if len(fragments) > 0 {
		add(0)
	}
	order := make([]int, len(fragments))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		if fragments[a].score != fragments[b].score {
			return fragments[b].score - fragments[a].score
		}
		return b - a
	})
	for _, i := range order {
		add(i)
	}
	blocks := []Block{}
	refs := []editorialFragment{}
	for i, f := range fragments {
		if selected[i] {
			blocks = append(blocks, Block{f.ID, f.text})
			refs = append(refs, f.editorialFragment)
		}
	}
	return blocks, refs, total - used
}

// Convert bounded model fragments back into ordinary document patches. Hidden
// prefixes/suffixes are spliced by the server, never reconstructed by the model.
// New text inside a fragment is kept within the original rich-text block; no
// synthetic fragment identity can enter persistence or move anchored media.
func expandEditorialRevision(r Revision, d rewriteDocument, original Snapshot) (Revision, error) {
	if d.ContextWindow == nil || len(d.ContextWindow.Fragments) == 0 {
		return r, nil
	}
	scoped := original
	scoped.Blocks = d.Blocks
	scoped.MediaOnlyBlockIDs = nil
	if err := r.Validate(scoped); err != nil {
		return r, err
	}
	replacements := map[string]string{}
	children := map[string][]string{}
	texts := map[string]string{}
	for _, p := range r.Patches {
		if p.AfterID == "" {
			replacements[p.ID] = p.Text
		} else {
			children[p.AfterID] = append(children[p.AfterID], p.ID)
			texts[p.ID] = p.Text
		}
	}
	// Revision.Validate guarantees each insertion points to an earlier known
	// node, so this graph cannot cycle. A stack also avoids recursive depth.
	additions := func(id string) string {
		var out strings.Builder
		stack := slices.Clone(children[id])
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			out.WriteString("\n\n")
			out.WriteString(texts[n])
			stack = append(stack, children[n]...)
		}
		return out.String()
	}
	refs := map[string][]editorialFragment{}
	for _, f := range d.ContextWindow.Fragments {
		refs[f.BlockID] = append(refs[f.BlockID], f)
	}
	result := r
	result.Patches = []Patch{}
	for _, b := range original.Blocks {
		chars := []rune(b.Text)
		cursor := 0
		changed := false
		var out strings.Builder
		for _, f := range refs[b.ID] {
			out.WriteString(string(chars[cursor:f.Start]))
			replacement, ok := replacements[f.ID]
			if ok {
				out.WriteString(replacement)
				changed = true
			} else {
				out.WriteString(string(chars[f.Start:f.End]))
			}
			extra := additions(f.ID)
			out.WriteString(extra)
			changed = changed || extra != ""
			cursor = f.End
		}
		out.WriteString(string(chars[cursor:]))
		if changed && out.String() != b.Text {
			result.Patches = append(result.Patches, Patch{ID: b.ID, Text: out.String()})
		}
	}
	return result, result.Validate(original)
}

const incrementalWindowInstructions = `
本轮为有边界的实时增量整理。contextWindow.transcriptStart 之前的旧口述已经由 App 接收过整理结果，本轮省略；保留的转写 start 仍是完整录音的绝对文字位置，不能重新从零理解手改先后。近期转写包含为衔接保留的旧话，不要重复添加。
contextWindow.fragments 非空时，blocks 是从原正文抽取的原文片段，id 是本轮临时片段标识；fragments 的 blockID/start/end 只说明它在原段落中的位置。这些片段之间可能省略了大量已整理正文，不能把所见片段拼接成一篇新全文。只改需要变化的片段，patch.id 和 afterID 只使用 blocks 中的临时 id 或本轮新建 id；服务端保留未提供的全部文字和媒体，并把修改放回原位置。不要复写遗漏的内容、补写承上启下的虚构经历或重写开头结尾。手改记录仍按原 blockID 对应片段，具体手改必须保留。若补充或纠正指向本轮未提供的旧事件，且现有片段不足以确定目标，在 questions 提示需核对该处，不能猜测旧正文或把矛盾新版本追加到末尾。`

func acceptsEditorialPatches(blocks []Block, patches []Patch) bool {
	text := map[string]string{}
	for _, b := range blocks {
		text[b.ID] = b.Text
	}
	for _, p := range patches {
		if value, ok := text[p.ID]; !ok || value != p.Text {
			return false
		}
	}
	return true
}
