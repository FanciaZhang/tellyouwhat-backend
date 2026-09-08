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
		return "自然记录：轻度理顺口述和句子衔接，尽量保留用户自己的用词、语气和叙述节奏。不要刻意修饰。风格示例（不是本篇事实）：口述“下班了，去河边走走，买了热茶，坐一会儿，轻松了点”→“下班后，我去河边走了走，买了一杯热茶。坐了一会儿，心情轻松了一些。”", nil
	case StyleLively:
		return "轻快活泼：主动把拖长的口述串句拆成有停顿感的短句，使用轻快、自然的口语节奏，像向朋友讲述经历；不要原封不动保留一长串“然后、之后”。不要强加兴奋语气，不新增笑话、表情符号、感叹或用户没有表达的情绪。风格示例（不是本篇事实）：口述“下班了，去河边走走，买了热茶，坐一会儿，轻松了点”→“下班了，去河边走走。我买了杯热茶，坐了一会儿。心情也轻松了些。”", nil
	case StyleDocumentary:
		return "严谨纪实：使用平实准确的完整书面句，去除口语填充和反复自我纠正的过程，直接采用已确认的正确说法，清楚交代已知的时间、人物、行为与经过。感受作为用户的主观感受保留，不改写成客观事实，不虚构因果。风格示例（不是本篇事实）：口述“下班了，去河边走走，买了热茶，坐一会儿，轻松了点”→“下班后，我到河边散步，买了一杯热茶，随后坐下休息了一会儿。心情比之前轻松。”", nil
	case StyleDaybook:
		return "日常流水：保留生活琐事和细节，按已知时间顺序一件接一件写；时间不明就保留叙述顺序，不猜测。把散步、买茶、休息等独立小事拆成短句逐项记下，少用过渡修饰，不把它们揉成一个长句。不要挑选重点或提炼主题，不写项目符号式清单。风格示例（不是本篇事实）：口述“下班了，去河边走走，买了热茶，坐一会儿，轻松了点”→“下班后去了河边，走了一会儿。买了一杯热茶。接着坐了一会儿，心情轻松了一些。”", nil
	case StyleEssay:
		return "散文随笔：正文应读起来像随笔：把已有动作和感受组织成长短交错的句子，用停顿或分段体现叙述节奏。资料已有细节时，用这些具体细节承接感受；只能使用资料中已有的意象，不添加景物、心理活动、比喻所隐含的新事实或升华感悟。主动调整句式与停顿，避免照搬口述的逗号串句；不要过度文学化。风格示例（不是本篇事实）：口述“下班了，去河边走走，买了热茶，坐一会儿，轻松了点”→“下班后，我沿着河边走了走。一杯热茶，片刻闲坐，心情也慢慢轻松了一些。”", nil
	default:
		return "", ErrInvalid
	}
}
