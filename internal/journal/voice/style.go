package voice

// WritingStyle is a closed identifier, never a client-supplied instruction.
type WritingStyle string

const (
	StyleNatural     WritingStyle = "natural"
	StyleLively      WritingStyle = "lively"
	StyleDocumentary WritingStyle = "documentary"
	StyleDaybook     WritingStyle = "daybook"
	StyleEssay       WritingStyle = "essay"
)

func (s WritingStyle) instructions() (string, error) {
	switch s {
	case "", StyleNatural: // An unspecified preference uses the product default.
		return "自然记录：轻度理顺口述和句子衔接，尽量保留用户自己的用词、语气和叙述节奏。不要刻意修饰。", nil
	case StyleLively:
		return "轻快活泼：使用轻快、自然的口语短句，像向朋友讲述经历。不要强加兴奋语气，不新增笑话、表情符号、感叹或用户没有表达的情绪。", nil
	case StyleDocumentary:
		return "严谨纪实：措辞平实准确，清楚交代已知的时间、人物、行为与经过。感受作为用户的主观感受保留，不改写成客观事实，不虚构因果。", nil
	case StyleDaybook:
		return "日常流水：保留生活琐事和细节，按已知时间顺序一件接一件写；时间不明就保留叙述顺序，不猜测。不要挑选重点或提炼主题，不写项目符号式清单。", nil
	case StyleEssay:
		return "散文随笔：让句子有自然的节奏，让已经说出的细节和感受顺畅衔接；只能使用资料中已有的意象，不添加景物、心理活动、比喻所隐含的新事实或升华感悟。不要过度文学化。", nil
	default:
		return "", ErrInvalid
	}
}
