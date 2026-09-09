package voice

import (
	"strings"
	"unicode/utf8"
)

// Names/narrator are caller-selected identities, never inferred from pitch,
// gender, or a provider's connection-local speaker number.
type RecordingSpeaker struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type RecordingContext struct {
	Mode              string             `json:"mode"`
	NarratorSpeakerID string             `json:"narratorSpeakerID"`
	Speakers          []RecordingSpeaker `json:"speakers"`
	Analysis          RecordingAnalysis  `json:"analysis"`
}

func (r RecordingContext) Validate(transcript string) error {
	if r.Mode != "narrative" && r.Mode != "dialogue" {
		return ErrInvalid
	}
	if r.Analysis.Version != RecordingAnalysisVersion || r.Analysis.Milliseconds <= 0 || r.Analysis.Milliseconds > SessionMilliseconds || r.Analysis.Text != transcript || len(r.Speakers) > 32 || len(r.Analysis.Utterances) > 10000 {
		return ErrInvalid
	}
	names := map[string]bool{}
	for _, p := range r.Speakers {
		if p.ID == "" || len(p.ID) > 128 || names[p.ID] || strings.TrimSpace(p.Name) == "" || utf8.RuneCountInString(p.Name) > 80 {
			return ErrInvalid
		}
		names[p.ID] = true
	}
	if r.NarratorSpeakerID != "" && !names[r.NarratorSpeakerID] {
		return ErrInvalid
	}
	seen := map[string]bool{}
	last := -1
	characters := 0
	for _, u := range r.Analysis.Utterances {
		characters += utf8.RuneCountInString(u.Text)
		if u.ID == "" || len(u.ID) > 128 || seen[u.ID] || len(u.Speaker) > 128 || u.StartMilliseconds < 0 || u.StartMilliseconds < last || u.EndMilliseconds <= u.StartMilliseconds || u.EndMilliseconds > r.Analysis.Milliseconds || characters > MaxContextCharacters || utf8.RuneCountInString(u.AcousticEmotion) > 256 || strings.TrimSpace(u.Text) == "" {
			return ErrInvalid
		}
		last = u.StartMilliseconds
		seen[u.ID] = true
	}
	return nil
}

// This is an evidence summary for confirmation UI, not a diarization correction.
// Short acknowledgements/laughter remain available and retain their source IDs;
// they must not manufacture an extra person or be silently merged into another.
type RecordingSpeakerEvidence struct {
	SpeakerID        string   `json:"speakerID"`
	UtteranceIDs     []string `json:"utteranceIDs"`
	RepresentativeID string   `json:"representativeID"`
	HasLexicalSpeech bool     `json:"hasLexicalSpeech"`
}

func (r RecordingAnalysis) SpeakerEvidence() []RecordingSpeakerEvidence {
	result := []RecordingSpeakerEvidence{}
	indices := map[string]int{}
	longest := map[string]int{}
	for _, u := range r.Utterances {
		if u.Speaker == "" {
			continue
		}
		i, ok := indices[u.Speaker]
		if !ok {
			i = len(result)
			indices[u.Speaker] = i
			result = append(result, RecordingSpeakerEvidence{SpeakerID: u.Speaker, UtteranceIDs: []string{}})
		}
		e := &result[i]
		e.UtteranceIDs = append(e.UtteranceIDs, u.ID)
		lexical := strings.Trim(u.Text, " \t\n\r嗯啊哦喔噢唔哈呵嘿，。？！?!、….") != ""
		e.HasLexicalSpeech = e.HasLexicalSpeech || lexical
		score := utf8.RuneCountInString(u.Text)
		if lexical {
			score += 100000
		}
		if e.RepresentativeID == "" || score > longest[u.Speaker] {
			longest[u.Speaker] = score
			e.RepresentativeID = u.ID
		}
	}
	return result
}

const recordingRewriteInstructions = `
本次含有 recordingContext：这是整次录音的带人物发言资料；按 startMilliseconds 理解发言顺序，人物身份只来自 speakers 映射，叙述视角只来自 narratorSpeakerID，不根据声音编号推断任何亲属关系。未映射的说话人不猜身份；短应答和笑声不足以证明出现了新人。整理文本只以已表达的事实为依据。
mode=narrative 时写成手记：以选定叙述者第一人称组织共同经历，但其他人说的“我”、身体感受、害怕、想法和行动都仍属于那个人；叙述者未参与的经历写为对方讲述，不能写成亲历。narratorSpeakerID 为空时用中性第三人称，不擅自指定“我”。mode=dialogue 时保留发言顺序、交替出现的人物和称呼，清理口头重复，不转换为独白，不增加议程或任务分派。
acousticEmotion 仅是录音时声音的未经确认分析，既不是事件当时心情，也不是心理事实。不得把这些标签改写成叙述者或其他人的感受；不得据此添加“笑着说”“哽咽着说”等描写。对原话明确说出的害怕、生气、欣慰、开心，按原话主体与所指时间保留，不受相反的声音标签影响。不得从情绪标签猜测身体健康或精神状态。
转述医生、医院、商家时保留“医生说”“我们觉得”等归属；说话人的推测、玩笑和夸张不能写成医学结论、因果事实或第三方已经证实的行为。对照前后内容理解显然的玩笑，不确定时省去其推断而保留确实发生的事情，或在 questions 提问。带有“好像、可能”的数字和时间不能改成确定结论。人物资料不改变现有手改保护和版本校验规则。
逐条落实主体：一个人的害怕、观察、发现、赞许、自嘲只能归给这个人；共同在场不等于共同感受，不能把“她害怕”扩成“我们吓了一跳”，也不能把她自嘲的话写成叙述者对她的贬低。人物归属不确定的短应答或笑声不用于推断身份或情绪。
不新增“笑说、笑着说、打趣说、哽咽着说”等表演性描述，录音里的笑声也不代表事件发生时在笑。明确的玩笑若保留，只说明是调侃，不写成真实因果，不借机扩写。
所有不确定修饰词必须跟随其事实保留，不能因整理流畅而删除“好像、大概、可能、记不清”。解释是谁提出的，就归给谁；说话人自己的解释不能变成医生、工作人员已经作出的说明。涉及健康、商家质量、规则、政策等陈述，忠实保留讲述者的理解与转述边界，不替他们断言可靠性、合法性、诊断或因果。`
