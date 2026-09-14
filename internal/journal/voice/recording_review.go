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

var recordingMedicalCertainty = []string{
	"已经确诊", "确诊为", "证明是", "完全正常", "没有问题", "无需复查", "必须治疗",
}

func uncertainRecordingNumbers(s string) map[string]bool {
	numbers := map[string]bool{}
	for _, pattern := range []*regexp.Regexp{recordingUncertainBefore, recordingUncertainAfter} {
		for _, match := range pattern.FindAllStringSubmatch(s, -1) {
			numbers[match[1]] = true
		}
	}
	return numbers
}

// Narrow, explainable review signals, not a proof of semantic equivalence.
// They catch numeric qualifiers lost in a draft, new digit-based facts, and a
// short list of unsupported medical-certainty phrases. Spelled-out numbers,
// names and broader meaning still require model evaluation or user review. The
// checker never rewrites a fact or substitutes its own guess for the source.
func ReviewRecordingDraft(a RecordingAnalysis, draft string) []RecordingReviewIssue {
	issues := []RecordingReviewIssue{}
	issueCodes := map[string]bool{}
	sourceText := ""
	sourceNumbers := map[string]bool{}
	for _, utterance := range a.Utterances {
		sourceText += utterance.Text
		for _, number := range recordingNumber.FindAllString(utterance.Text, -1) {
			sourceNumbers[number] = true
		}
	}
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
				issueCodes["numeric_uncertainty_needs_review"] = true
				break
			}
		}
	}
	for _, number := range recordingNumber.FindAllString(draft, -1) {
		if !sourceNumbers[number] && !issueCodes["ungrounded_number_needs_review"] {
			issues = append(issues, RecordingReviewIssue{"", "ungrounded_number_needs_review", ""})
			issueCodes["ungrounded_number_needs_review"] = true
		}
	}
	for _, phrase := range recordingMedicalCertainty {
		if strings.Contains(draft, phrase) && !strings.Contains(sourceText, phrase) &&
			!issueCodes["ungrounded_medical_certainty_needs_review"] {
			issues = append(issues, RecordingReviewIssue{"", "ungrounded_medical_certainty_needs_review", ""})
			issueCodes["ungrounded_medical_certainty_needs_review"] = true
		}
	}
	return issues
}
