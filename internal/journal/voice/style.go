package voice

// WritingStyle is a stable catalog identifier, never a client-supplied instruction.
type WritingStyle string

const (
	StyleNatural     WritingStyle = "natural"
	StyleLively      WritingStyle = "lively"
	StyleDocumentary WritingStyle = "documentary"
	StyleDaybook     WritingStyle = "daybook"
	StyleEssay       WritingStyle = "essay"
)
