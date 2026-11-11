package promptconfig

import (
	"strings"
	"testing"
)

func TestFinalizationPromptIsOptionalAndBounded(t *testing.T) {
	policy := Defaults("lite", "pro", "voice", 90)["journal"]
	if err := policy.Validate("journal"); err != nil {
		t.Fatal("default finalization changed", err)
	}
	policy.Journal.Voice.PolishPrompt = "去除口头重复，保留原文细节。"
	if err := policy.Validate("journal"); err != nil {
		t.Fatal(err)
	}
	policy.Journal.Voice.PolishPrompt = strings.Repeat("x", 16001)
	if err := policy.Validate("journal"); err == nil {
		t.Fatal("unbounded prompt accepted")
	}
}
