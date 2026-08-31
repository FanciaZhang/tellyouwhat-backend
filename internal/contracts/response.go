package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

var unquotedStringValuePattern = regexp.MustCompile(`"([^"\\]+)"\s*:\s*([^"{}\[\],\s]+)"`)
var foodGroupSynonymPattern = regexp.MustCompile(`"group"\s*:\s*"([^"]+)"`)

var foodGroupSynonyms = map[string]string{
	"legume": "soy", "legumes": "soy", "pulse": "soy", "pulses": "soy",
	"bean": "soy", "beans": "soy", "soybean": "soy", "soybeans": "soy",
	"大豆": "soy", "豆类": "soy", "豆制品": "soy",
	"cereal": "grains", "cereals": "grains", "whole_grain": "grains", "whole_grains": "grains", "谷物": "grains",
	"root_vegetable": "tubers", "root_vegetables": "tubers", "starchy_vegetable": "tubers", "starchy_vegetables": "tubers", "薯类": "tubers",
	"vegetable": "vegetables", "蔬菜": "vegetables",
	"fruit": "fruits", "水果": "fruits",
	"fish": "fish", "鱼类": "fish",
	"shellfish": "seafood", "海鲜": "seafood",
	"redmeat": "red_meat", "红肉": "red_meat",
	"poultry": "poultry", "禽肉": "poultry",
	"egg": "eggs", "蛋类": "eggs",
	"dairy_product": "dairy", "dairy_products": "dairy", "乳制品": "dairy", "奶制品": "dairy",
	"nut": "nuts", "nuts": "nuts", "坚果": "nuts",
	"其他": "other",
}

// RepairUnquotedStringValues fixes the common model mistake of dropping the
// opening quote of a string value, e.g. `"estimated_amount":1份"`. It only
// runs when the whole content is invalid JSON, never rewrites number/boolean/
// null literals, and only returns a repair when the result parses as JSON.
func RepairUnquotedStringValues(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" || json.Valid([]byte(trimmed)) {
		return content, false
	}
	matches := unquotedStringValuePattern.FindAllStringSubmatchIndex(trimmed, -1)
	if len(matches) == 0 {
		return content, false
	}
	var builder strings.Builder
	cursor := 0
	changed := false
	replacements := 0
	for _, match := range matches {
		if len(match) < 6 {
			continue
		}
		keyStart, keyEnd := match[2], match[3]
		tokenStart, tokenEnd := match[4], match[5]
		token := trimmed[tokenStart:tokenEnd]
		if isJSONLiteral(token) {
			continue
		}
		builder.WriteString(trimmed[cursor:match[0]])
		builder.WriteString(`"` + trimmed[keyStart:keyEnd] + `":"` + token + `"`)
		cursor = match[1]
		changed = true
		replacements++
		if replacements >= 64 {
			break
		}
	}
	if !changed {
		return content, false
	}
	builder.WriteString(trimmed[cursor:])
	repaired := builder.String()
	if !json.Valid([]byte(repaired)) {
		return content, false
	}
	return repaired, true
}

func isJSONLiteral(token string) bool {
	var value any
	if err := json.Unmarshal([]byte(token), &value); err != nil {
		return false
	}
	switch value.(type) {
	case float64, bool, nil:
		return true
	}
	return false
}

// CanonicalizeFoodGroupSynonyms rewrites common model synonyms in
// diet_evidence.food_group_contributions (e.g. "legumes" -> "soy") so the
// closed schema enum can be enforced locally without discarding a usable meal.
func CanonicalizeFoodGroupSynonyms(content string) (string, bool) {
	matches := foodGroupSynonymPattern.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return content, false
	}
	var builder strings.Builder
	cursor := 0
	changed := false
	replacements := 0
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		valueStart, valueEnd := match[2], match[3]
		rawValue := strings.TrimSpace(content[valueStart:valueEnd])
		canonical, ok := foodGroupSynonyms[strings.ToLower(rawValue)]
		if !ok || canonical == rawValue {
			continue
		}
		builder.WriteString(content[cursor:match[0]])
		builder.WriteString(`"group":"` + canonical + `"`)
		cursor = match[1]
		changed = true
		replacements++
		if replacements >= 64 {
			break
		}
	}
	if !changed {
		return content, false
	}
	builder.WriteString(content[cursor:])
	return builder.String(), true
}

func ValidateResponse(request Request, content string) error {
	var schemaDocument any
	if err := json.Unmarshal(request.ResponseSchema, &schemaDocument); err != nil || containsRemoteReference(schemaDocument) {
		return fmt.Errorf("%w: invalid response schema", ErrContractViolation)
	}
	compiler := jsonschema.NewCompiler()
	const schemaURL = "urn:health:response-schema"
	if err := compiler.AddResource(schemaURL, schemaDocument); err != nil {
		return fmt.Errorf("%w: compile response schema", ErrContractViolation)
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		return fmt.Errorf("%w: compile response schema", ErrContractViolation)
	}
	var instance any
	decoder := json.NewDecoder(bytes.NewBufferString(content))
	if err := decoder.Decode(&instance); err != nil {
		return fmt.Errorf("%w: response is not JSON", ErrContractViolation)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("%w: response does not match schema", ErrContractViolation)
	}
	return nil
}

func containsRemoteReference(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "$ref" {
				reference, ok := child.(string)
				if !ok || (!strings.HasPrefix(reference, "#/") && reference != "#") {
					return true
				}
			}
			if containsRemoteReference(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsRemoteReference(child) {
				return true
			}
		}
	}
	return false
}

