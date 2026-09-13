package voice

import (
	"regexp"
	"strings"
)

type RecordingReviewIssue struct {
	UtteranceID string `json:"utteranceID"`
	Code        string `json:"code"`
	SourceText  string `json:"sourceText"`
}

var recordingNumber = regexp.MustCompile(`[0-9]+(?:[.:][0-9]+)*`)

var recordingUncertainBefore = regexp.MustCompile(`(?:好像|大概|可能|记不清|不确定|似乎|大约)[^。！？；，\n0-9]{0,4}([0-9]+(?:[.:][0-9]+)*)`)
var recordingUncertainAfter = regexp.MustCompile(`([0-9]+(?:[.:][0-9]+)*)(?:元|块钱|块|斤|公斤|周|天|个|点|分|秒)?(?:好像|大概|大约)`)

func uncertainRecordingNumbers(s string) map[string]bool {
	numbers := map[string]bool{}
	for _, pattern := range []*regexp.Regexp{recordingUncertainBefore, recordingUncertainAfter} {
		for _, match := range pattern.FindAllStringSubmatch(s, -1) {
			numbers[match[1]] = true
		}
	}
	return numbers
}

// A narrow, explainable review signal, not a proof of semantic equivalence.
// This catches written numeric qualifiers lost in a draft. Spelled-out number
// conversion and nonnumeric uncertainty still require semantic/user review.
// It never rewrites a fact or substitutes its own guess for the source.
func ReviewRecordingDraft(a RecordingAnalysis, draft string) []RecordingReviewIssue {
	issues := []RecordingReviewIssue{}
	sentences := strings.FieldsFunc(draft, func(r rune) bool { return r == '。' || r == '！' || r == '？' || r == '\n' })
	definiteNumbers := map[string]bool{}
	for _, sentence := range sentences {
		uncertain := uncertainRecordingNumbers(sentence)
		for _, number := range recordingNumber.FindAllString(sentence, -1) {
			if !uncertain[number] {
				definiteNumbers[number] = true
			}
		}
	}
	for _, u := range a.Utterances {
		for number := range uncertainRecordingNumbers(u.Text) {
			if definiteNumbers[number] {
				issues = append(issues, RecordingReviewIssue{u.ID, "numeric_uncertainty_needs_review", u.Text})
				break
			}
		}
	}
	return issues
}
