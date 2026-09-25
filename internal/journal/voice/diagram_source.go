package voice

import (
	"strings"
)

// Historical content can explain a graph, but only current speech can authorize
// its creation. Exact partition coverage prevents instructions becoming facts.
func diagramSourceAuthorized(source TableSource, role, blockID string, r Revision, s Snapshot, historical []TableSource) bool {
	if role != "content" && role != "instruction" {
		return false
	}
	if !validID(source.SourceID) || !validID(blockID) || strings.TrimSpace(source.Anchor.Quote) == "" {
		return false
	}
	consumed := 0
	for _, id := range r.ConsumedSourceIDs {
		if strings.EqualFold(id, source.SourceID) {
			consumed++
		}
	}
	if consumed == 0 && role == "content" {
		if validateStructuredSourceContext(historical, s) != nil {
			return false
		}
		for _, p := range r.SourcePartitions {
			if strings.EqualFold(p.SourceID, source.SourceID) {
				return false
			}
		}
		for _, old := range historical {
			if strings.EqualFold(old.SourceID, source.SourceID) {
				if _, _, ok := tableAnchorRange(old.Anchor.Quote, source.Anchor); ok {
					return true
				}
			}
		}
		return false
	}
	if consumed != 1 {
		return false
	}
	text, matches := "", 0
	for _, turn := range s.PendingUtterances {
		if strings.EqualFold(turn.ID, source.SourceID) {
			text = turn.Text
			matches++
		}
	}
	if matches != 1 {
		return false
	}
	start, end, ok := tableAnchorRange(text, source.Anchor)
	if !ok {
		return false
	}
	partitions, authorized := 0, false
	for _, p := range r.SourcePartitions {
		if !strings.EqualFold(p.SourceID, source.SourceID) {
			continue
		}
		partitions++
		joined, offset := "", 0
		for _, seg := range p.Segments {
			if seg.Text == "" {
				return false
			}
			next, linked := offset+len(seg.Text), false
			for _, id := range seg.BlockIDs {
				if strings.EqualFold(id, blockID) {
					linked = true
				}
			}
			if linked && seg.Role == role && start >= offset && end <= next &&
				(role != "instruction" || start == offset && end == next) {
				authorized = true
			}
			joined += seg.Text
			offset = next
		}
		if joined != text {
			return false
		}
	}
	return partitions == 1 && authorized
}
