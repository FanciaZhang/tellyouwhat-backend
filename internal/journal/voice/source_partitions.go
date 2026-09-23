package voice

import "strings"

// Validate exact source coverage and provenance before delivering model output.
// The app additionally verifies grapheme boundaries against its native text model.
func validateSourcePartitions(r Revision, s Snapshot) error {
	if len(r.SourcePartitions) > MaxPendingUtterances {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, p := range r.SourcePartitions {
		if seen[p.SourceID] || len(p.Segments) == 0 || len(p.Segments) > 128 {
			return ErrInvalid
		}
		seen[p.SourceID] = true
		source := ""
		for _, u := range s.PendingUtterances {
			if u.ID == p.SourceID {
				source = u.Text
			}
		}
		if source == "" {
			return ErrInvalid
		}
		passages, corrections, linked := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, passage := range r.Passages {
			for _, id := range passage.SourceIDs {
				if id == p.SourceID {
					passages[passage.BlockID] = true
				}
			}
		}
		for _, c := range r.Corrections {
			for _, id := range c.EvidenceSourceIDs {
				if id == p.SourceID {
					corrections[c.BlockID] = true
				}
			}
		}
		type instruction struct {
			start, end int
			blocks     map[string]bool
		}
		instructions := map[string]*instruction{}
		add := func(text, block string) {
			if existing := instructions[text]; existing != nil {
				existing.blocks[block] = true
				return
			}
			start := strings.Index(source, text)
			instructions[text] = &instruction{start, start + len(text), map[string]bool{block: true}}
		}
		for _, c := range r.FormatCommands {
			if c.SourceID == p.SourceID {
				add(c.Instruction, c.BlockID)
			}
		}
		for _, c := range r.MoveCommands {
			if c.SourceID == p.SourceID {
				for _, id := range moveTargets(c, s) {
					add(c.Instruction, id)
				}
			}
		}
		for _, c := range r.FormatResolutions {
			if c.SourceID == p.SourceID {
				for _, item := range s.FormatContext {
					if item.ReceiptID == c.ReceiptID {
						add(c.Instruction, item.BlockID)
						for _, id := range item.AdditionalBlockIDs {
							add(c.Instruction, id)
						}
					}
				}
			}
		}
		offset := 0
		for _, segment := range p.Segments {
			if segment.Text == "" || !strings.HasPrefix(source[offset:], segment.Text) {
				return ErrInvalid
			}
			end := offset + len(segment.Text)
			blocks := map[string]bool{}
			for _, id := range segment.BlockIDs {
				if blocks[id] || !validID(id) {
					return ErrInvalid
				}
				blocks[id] = true
			}
			for text, evidence := range instructions {
				if offset < evidence.end && end > evidence.start {
					if segment.Role != "instruction" || text != segment.Text || offset != evidence.start {
						return ErrInvalid
					}
				}
			}
			switch segment.Role {
			case "content", "organization":
				if len(blocks) == 0 {
					return ErrInvalid
				}
				for id := range blocks {
					if !passages[id] {
						return ErrInvalid
					}
					linked[id] = true
				}
			case "correction":
				if len(blocks) == 0 {
					return ErrInvalid
				}
				for id := range blocks {
					if !corrections[id] {
						return ErrInvalid
					}
				}
			case "context":
				if len(blocks) != 0 {
					return ErrInvalid
				}
			case "instruction":
				evidence := instructions[segment.Text]
				if evidence == nil || evidence.start != offset || len(evidence.blocks) != len(blocks) {
					return ErrInvalid
				}
				for id := range blocks {
					if !evidence.blocks[id] {
						return ErrInvalid
					}
				}
			default:
				return ErrInvalid
			}
			offset = end
		}
		if offset != len(source) || len(linked) != len(passages) {
			return ErrInvalid
		}
	}
	return nil
}
