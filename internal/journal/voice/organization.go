package voice

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const OrganizationVersion = "journal-voice-organization-v1"
const MaxOrganizationBytes = 2 * 1024 * 1024

// Each section is separated by a manual or non-text block in the manuscript.
// Models may combine speech within a section, never across those boundaries.
type OrganizationPlan struct {
	Version  string     `json:"version"`
	Sections []Snapshot `json:"sections"`
}

type OrganizationState struct {
	Section    int
	Next       int
	Results    []PolishRevision
	Paragraphs []PolishParagraph
	Questions  []string
	Omitted    map[string][]PolishOmission
}

type organizationFragment struct {
	Target    PolishTarget
	Originals []string
}

type OrganizationWork struct {
	Snapshot  Snapshot
	Next      int
	mapping   map[string][]string
	retained  int
	fragments []organizationFragment
}

func (p OrganizationPlan) Validate() error {
	if p.Version != OrganizationVersion || len(p.Sections) == 0 || len(p.Sections) > 256 {
		return ErrInvalid
	}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > MaxOrganizationBytes {
		return ErrInvalid
	}
	ids, sources, count, targets := map[string]bool{}, map[string]SourceUtterance{}, 0, 0
	for _, section := range p.Sections {
		request := section.Polish
		if request == nil || request.Lightweight || request.Paused || request.Deferred || section.Identity != nil ||
			section.RecordingContext != nil || section.Revision < 0 || len(section.Words) > 32 ||
			len(request.Targets) == 0 || len(request.Context) > 2 || len(request.ReusableContextIDs) > 0 || validateNarrator(request.Narrator) != nil {
			return ErrInvalid
		}
		for _, t := range request.Targets {
			targets++
			if uuid.Validate(t.ID) != nil || ids[t.ID] || !validPolishStyle(t.Style) ||
				len(t.Turns) > 512 || len(t.SourceIDs) > 512 ||
				len(t.SourceKeys) != 0 && len(t.SourceKeys) != len(t.Turns) ||
				len(t.SourceOffsets) != 0 && len(t.SourceOffsets) != len(t.Turns) {
				return ErrInvalid
			}
			ids[t.ID] = true
			for _, id := range t.SourceIDs {
				if uuid.Validate(id) != nil {
					return ErrInvalid
				}
			}
			for _, text := range []string{t.Text, t.SourceText, t.RetainedText} {
				if utf8.RuneCountInString(text) > MaxContextCharacters {
					return ErrInvalid
				}
			}
			for _, key := range t.SourceKeys {
				if uuid.Validate(key) != nil {
					return ErrInvalid
				}
			}
			for _, offset := range t.SourceOffsets {
				if offset < 0 || offset > MaxContextCharacters {
					return ErrInvalid
				}
			}
			if len(t.Turns) == 0 {
				count += utf8.RuneCountInString(t.SourceText) + utf8.RuneCountInString(t.RetainedText)
			}
			for _, u := range t.Turns {
				if uuid.Validate(u.ID) != nil || !slices.Contains(t.SourceIDs, u.ID) || !validPersonID(u.PersonID) || !u.validExclusions() ||
					len(u.Person) > 240 || len(u.Speaker) > 200 || u.StartMilliseconds < 0 || u.EndMilliseconds < u.StartMilliseconds || u.EndMilliseconds > SessionMilliseconds {
					return ErrInvalid
				}
				if previous, found := sources[u.ID]; found {
					if !previous.Equal(u) {
						return ErrInvalid
					}
				} else {
					sources[u.ID] = u
					count += utf8.RuneCountInString(u.Text)
				}
			}
		}
		// Validate the same bounded requests that will actually reach a model.
		state := OrganizationState{}
		fragments := organizationFragments(section)
		if len(fragments) == 0 || len(fragments) > 1024 {
			return ErrInvalid
		}
		for state.Next < len(fragments) {
			work, err := state.work(section, fragments)
			if err != nil || work.Next <= state.Next || work.Snapshot.Validate() != nil {
				return ErrInvalid
			}
			state.Next = work.Next
		}
	}
	if targets > 1024 || count == 0 || count > MaxContextCharacters {
		return ErrInvalid
	}
	return nil
}

func organizationFragments(section Snapshot) []organizationFragment {
	targets := section.Polish.Targets
	parent := make([]int, len(targets))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	owners := map[string]int{}
	for i, t := range targets {
		for _, id := range t.SourceIDs {
			if prior, ok := owners[id]; ok {
				parent[find(i)] = find(prior)
			} else {
				owners[id] = i
			}
		}
	}
	groups := map[int][]int{}
	order := []int{}
	for i := range targets {
		key := find(i)
		if groups[key] == nil {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	fragments := []organizationFragment{}
	for _, key := range order {
		originals, turns, seen := []string{}, []SourceUtterance{}, map[string]bool{}
		style, identityCorrection := "body", false
		retained := []string{}
		for _, i := range groups[key] {
			t := targets[i]
			if t.Style != "body" {
				style = t.Style
			}
			identityCorrection = identityCorrection || t.IdentityCorrection
			originals = append(originals, t.ID)
			if !t.CompleteSource && strings.TrimSpace(t.RetainedText) != "" {
				retained = append(retained, t.RetainedText)
			}
			for _, u := range t.Turns {
				if !seen[u.ID] {
					turns = append(turns, u)
					seen[u.ID] = true
				}
			}
			if len(t.Turns) == 0 && strings.TrimSpace(t.SourceText) != "" {
				turns = append(turns, SourceUtterance{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(t.ID+"/source")).String(), Text: t.SourceText})
			}
		}
		for n, text := range retained {
			id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(strings.Join(originals, "/")+fmt.Sprint("/retained/", n))).String()
			fragments = append(fragments, organizationFragment{Target: PolishTarget{ID: id, Text: text, RetainedText: text, Style: style, IdentityCorrection: identityCorrection}, Originals: originals})
		}
		for _, u := range turns {
			for offset, text := range splitOrganizationText(u.content()) {
				turn := u
				turn.Text = text
				turn.ExcludedText = nil
				id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(strings.Join(originals, "/")+u.ID+fmt.Sprint("/", offset))).String()
				fragments = append(fragments, organizationFragment{Target: PolishTarget{ID: id, Text: text, SourceText: text, Turns: []SourceUtterance{turn}, SourceIDs: []string{turn.ID}, Style: style, IdentityCorrection: identityCorrection, CompleteSource: true}, Originals: originals})
			}
		}
	}
	return fragments
}

func splitOrganizationText(text string) []string {
	result := []string{}
	chars := []rune(text)
	for len(chars) > 0 {
		end := min(900, len(chars))
		if end < len(chars) {
			for i := end - 1; i >= end/2; i-- {
				if strings.ContainsRune("。！？.!?；;\n", chars[i]) {
					end = i + 1
					break
				}
			}
		}
		if strings.TrimSpace(string(chars[:end])) != "" {
			result = append(result, string(chars[:end]))
		}
		chars = chars[end:]
	}
	return result
}

func (s OrganizationState) Work(p OrganizationPlan) (OrganizationWork, error) {
	if s.Section < 0 || s.Section >= len(p.Sections) {
		return OrganizationWork{}, ErrInvalid
	}
	section := p.Sections[s.Section]
	return s.work(section, organizationFragments(section))
}

func (s OrganizationState) work(section Snapshot, fragments []organizationFragment) (OrganizationWork, error) {
	if s.Next < 0 || s.Next >= len(fragments) {
		return OrganizationWork{}, ErrInvalid
	}
	request := PolishRequest{Narrator: section.Polish.Narrator, Context: slices.Clone(section.Polish.Context), SelectionContext: section.Polish.SelectionContext}
	work := OrganizationWork{mapping: map[string][]string{}}
	// Keep only bounded, already validated prose editable for continuation.
	for i := len(s.Paragraphs) - 1; i >= 0 && work.retained < 2; i-- {
		paragraph := s.Paragraphs[i]
		if utf8.RuneCountInString(paragraph.Text) > 600 {
			break
		}
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint("organization-context/", s.Section, "/", s.Next, "/", i))).String()
		request.Targets = append([]PolishTarget{{ID: id, Text: paragraph.Text, RetainedText: paragraph.Text, Style: paragraph.Style}}, request.Targets...)
		work.mapping[id] = slices.Clone(paragraph.TargetIDs)
		work.retained++
	}
	characters, seen := 0, map[string]bool{}
	for i := s.Next; i < len(fragments); i++ {
		f := fragments[i]
		n := utf8.RuneCountInString(f.Target.SourceText) + utf8.RuneCountInString(f.Target.RetainedText)
		duplicate := slices.ContainsFunc(f.Target.SourceIDs, func(id string) bool { return seen[id] })
		if len(work.fragments) > 0 && (characters+n > 900 || len(request.Targets) >= 8 || duplicate) {
			break
		}
		request.Targets = append(request.Targets, f.Target)
		work.fragments = append(work.fragments, f)
		work.mapping[f.Target.ID] = f.Originals
		for _, id := range f.Target.SourceIDs {
			seen[id] = true
		}
		characters += n
		work.Next = i + 1
	}
	work.Snapshot = Snapshot{Organization: true, Revision: section.Revision, WritingStyle: section.WritingStyle, Words: section.Words,
		DictationMode: true, Narrator: request.Narrator, Polish: &request, IllustrationSuggestionsEnabled: section.IllustrationSuggestionsEnabled}
	for _, b := range request.Context {
		work.Snapshot.Blocks = append(work.Snapshot.Blocks, b)
	}
	for _, t := range request.Targets {
		work.Snapshot.Blocks = append(work.Snapshot.Blocks, Block{ID: t.ID, Text: t.Text, Style: t.Style})
	}
	if err := work.Snapshot.Validate(); err != nil {
		return OrganizationWork{}, err
	}
	return work, nil
}

func (s *OrganizationState) Advance(plan OrganizationPlan, work OrganizationWork, result *PolishRevision) error {
	copy := *s
	copy.Omitted, copy.Questions, copy.Results = maps.Clone(s.Omitted), slices.Clone(s.Questions), slices.Clone(s.Results)
	if err := copy.advance(plan, work, result); err != nil {
		return err
	}
	*s = copy
	return nil
}

func (s *OrganizationState) advance(plan OrganizationPlan, work OrganizationWork, result *PolishRevision) error {
	if result == nil || result.Lightweight || result.RequiresFullOrganization || work.Next <= s.Next || s.Section >= len(plan.Sections) {
		return ErrInvalid
	}
	next := slices.Clone(s.Paragraphs[:len(s.Paragraphs)-work.retained])
	for _, p := range result.Paragraphs {
		ids := []string{}
		for _, id := range p.TargetIDs {
			originals, ok := work.mapping[id]
			if !ok {
				return ErrInvalid
			}
			for _, original := range originals {
				if !slices.Contains(ids, original) {
					ids = append(ids, original)
				}
			}
		}
		if len(ids) == 0 {
			return ErrInvalid
		}
		next = append(next, PolishParagraph{Text: p.Text, Style: p.Style, TargetIDs: ids})
	}
	if s.Omitted == nil {
		s.Omitted = map[string][]PolishOmission{}
	}
	for _, o := range result.Omissions {
		s.Omitted[o.SourceID] = append(slices.Clone(s.Omitted[o.SourceID]), o)
	}
	for _, q := range result.Questions {
		if len(s.Questions) < 8 && !slices.Contains(s.Questions, q) {
			s.Questions = append(s.Questions, q)
		}
	}
	s.Paragraphs, s.Next = next, work.Next
	section := plan.Sections[s.Section]
	if s.Next == len(organizationFragments(section)) {
		revision := PolishRevision{ID: uuid.NewString(), Targets: section.Polish.Targets, Paragraphs: append([]PolishParagraph{}, s.Paragraphs...), Questions: append([]string{}, s.Questions...), Narrator: section.Polish.Narrator, IllustrationSuggestion: result.IllustrationSuggestion, Omissions: []PolishOmission{}}
		sources := polishSources(*section.Polish)
		for _, source := range sources {
			omitted := s.Omitted[source.ID]
			parts := splitOrganizationText(source.content())
			complete, reason := len(omitted) == len(parts) && len(parts) > 0, ""
			for i, o := range omitted {
				if i >= len(parts) || strings.TrimSpace(o.Text) != strings.TrimSpace(parts[i]) {
					complete = false
				}
				if reason != "" && reason != o.Reason {
					return ErrInvalid
				}
				reason = o.Reason
			}
			if complete {
				revision.Omissions = append(revision.Omissions, PolishOmission{SourceID: source.ID, Text: source.Text, Reason: reason})
			}
		}
		characters := 0
		for _, p := range revision.Paragraphs {
			characters += utf8.RuneCountInString(p.Text)
		}
		if len(revision.Paragraphs) > 512 || characters > MaxContextCharacters {
			return ErrInvalid
		}
		s.Results = append(s.Results, revision)
		s.Section++
		s.Next = 0
		s.Paragraphs = nil
		s.Questions = nil
		s.Omitted = nil
	}
	return nil
}
