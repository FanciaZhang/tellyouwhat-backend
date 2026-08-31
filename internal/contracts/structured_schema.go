package contracts

import (
	"encoding/json"
	"fmt"
	"sort"
)

// allowedNutritionKeys mirrors Swift NutrientKey.allCases minus .energy.
// Keep in sync with HealthApp/Features/Diet/Models/Nutrition.swift.
var allowedNutritionKeys = map[string]struct{}{
	"protein":            {},
	"carbohydrates":      {},
	"sugar":              {},
	"fiber":              {},
	"fatTotal":           {},
	"fatSaturated":       {},
	"fatMonounsaturated": {},
	"fatPolyunsaturated": {},
	"cholesterol":        {},
	"water":              {},
	"caffeine":           {},
	"sodium":             {},
	"potassium":          {},
	"calcium":            {},
	"iron":               {},
	"magnesium":          {},
	"zinc":               {},
	"copper":             {},
	"manganese":          {},
	"selenium":           {},
	"iodine":             {},
	"phosphorus":         {},
	"chromium":           {},
	"molybdenum":         {},
	"chloride":           {},
	"vitaminA":           {},
	"vitaminC":           {},
	"vitaminD":           {},
	"vitaminE":           {},
	"vitaminK":           {},
	"thiamin":            {},
	"riboflavin":         {},
	"niacin":             {},
	"pantothenicAcid":    {},
	"vitaminB6":          {},
	"biotin":             {},
	"folate":             {},
	"vitaminB12":         {},
}

// V5 uses one food-centered root. V4 roots remain temporarily accepted by the
// gateway for already shipped clients, but new clients never parse them.
var mealRootVariants = map[Operation][][]string{
	OperationMealPhotoCapture: {
		{"foods", "meal_context", "water_candidates"},
		{"box", "default_recipe_variant_id", "food_context", "food_id", "grams", "image_index", "kcal", "name", "nutrition", "part_analysis", "recipe_variants"},
		{"components", "confidence", "containers", "context_analysis", "diet_evidence", "items", "plain_water_candidates", "summary"},
		{"consumption_components", "consumption_options", "grams", "kcal", "name", "nutrition"},
		{"basis", "capacity_ml", "confidence", "lower_bound_ml", "recognized", "suggested_name", "upper_bound_ml"},
	},
	OperationMealTextCapture: {
		{"foods", "meal_context", "water_candidates"},
		{"box", "default_recipe_variant_id", "food_context", "food_id", "grams", "image_index", "kcal", "name", "nutrition", "part_analysis", "recipe_variants"},
		{"components", "confidence", "containers", "context_analysis", "diet_evidence", "items", "summary"},
		{"consumption_components", "consumption_options", "grams", "kcal", "name", "nutrition"},
	},
}

var unsupportedSchemaKeywords = map[string]struct{}{
	"$ref": {}, "$id": {}, "$schema": {}, "pattern": {}, "format": {},
	"const": {}, "not": {}, "allOf": {}, "oneOf": {}, "if": {}, "then": {},
	"else": {}, "definitions": {}, "$defs": {},
}

const maxMealSchemaStringLength = 2048
const maxMealSchemaArrayItems = 64
const maxMealSchemaEnumValues = 256
const maxMealSchemaNumberMaximum = 3000

// ValidateStructuredMealSchema accepts only schemas that match the Swift-owned
// meal contract shapes with bounded strings/arrays/numbers, closed objects,
// and a nutrition allowlist. It replaces exact-hash whitelisting for
// per-user nutrition schemas.
func ValidateStructuredMealSchema(raw json.RawMessage, operation Operation) error {
	variants, ok := mealRootVariants[operation]
	if !ok {
		return fmt.Errorf("operation does not support a structured schema")
	}
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return fmt.Errorf("response schema is not a JSON object")
	}
	if err := validateNode(node, "$"); err != nil {
		return err
	}
	properties, _ := node["properties"].(map[string]any)
	actual := sortedKeys(properties)
	for _, variant := range variants {
		if equalStringSlices(actual, variant) {
			return nil
		}
	}
	return fmt.Errorf("response schema root does not match a registered meal shape")
}

func validateNode(node map[string]any, path string) error {
	for keyword := range node {
		if _, rejected := unsupportedSchemaKeywords[keyword]; rejected {
			return fmt.Errorf("%s: unsupported schema keyword %q", path, keyword)
		}
	}
	if anyOf, ok := node["anyOf"].([]any); ok {
		if len(anyOf) == 0 {
			return fmt.Errorf("%s: anyOf must not be empty", path)
		}
		for index, raw := range anyOf {
			branch, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.anyOf[%d]: branch must be an object", path, index)
			}
			if isNullOnly(branch) {
				continue
			}
			if err := validateNode(branch, fmt.Sprintf("%s.anyOf[%d]", path, index)); err != nil {
				return err
			}
		}
		return nil
	}

	types := nodeTypes(node)
	if len(types) == 0 {
		return fmt.Errorf("%s: schema type is required", path)
	}
	if containsString(types, "object") {
		return validateObjectNode(node, path)
	}
	if containsString(types, "array") {
		return validateArrayNode(node, path)
	}
	if containsString(types, "string") {
		return validateStringNode(node, path)
	}
	if containsString(types, "number") || containsString(types, "integer") {
		return validateNumberNode(node, path)
	}
	if containsString(types, "boolean") {
		return nil
	}
	return fmt.Errorf("%s: unsupported schema type", path)
}

func validateObjectNode(node map[string]any, path string) error {
	if !containsString(nodeTypes(node), "object") {
		return fmt.Errorf("%s: expected object type", path)
	}
	if additionalProperties, ok := node["additionalProperties"]; !ok || additionalProperties != false {
		return fmt.Errorf("%s: additionalProperties must be false", path)
	}
	properties, ok := node["properties"].(map[string]any)
	if !ok || len(properties) == 0 {
		return fmt.Errorf("%s: properties object is required", path)
	}
	required, err := stringArray(node, "required", path)
	if err != nil {
		return err
	}
	sort.Strings(required)
	if !equalStringSlices(required, sortedKeys(properties)) {
		return fmt.Errorf("%s: required must equal properties keys", path)
	}
	for name, raw := range properties {
		child, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s.%s: schema node must be an object", path, name)
		}
		childPath := path + "." + name
		if err := validateNode(child, childPath); err != nil {
			return err
		}
		if name == "nutrition" {
			nutritionProperties, _ := child["properties"].(map[string]any)
			for key := range nutritionProperties {
				if _, allowed := allowedNutritionKeys[key]; !allowed {
					return fmt.Errorf("%s: unknown nutrition field %q", childPath, key)
				}
			}
		}
	}
	return nil
}

func validateArrayNode(node map[string]any, path string) error {
	if !containsString(nodeTypes(node), "array") {
		return fmt.Errorf("%s: expected array type", path)
	}
	items, ok := node["items"].(map[string]any)
	if !ok {
		return fmt.Errorf("%s: array items schema is required", path)
	}
	maxItems, ok := node["maxItems"].(float64)
	if !ok || maxItems < 1 || maxItems > maxMealSchemaArrayItems {
		return fmt.Errorf("%s: maxItems must be between 1 and %d", path, maxMealSchemaArrayItems)
	}
	return validateNode(items, path+"[]")
}

func validateStringNode(node map[string]any, path string) error {
	if !containsString(nodeTypes(node), "string") {
		return fmt.Errorf("%s: expected string type", path)
	}
	maxLength, ok := node["maxLength"].(float64)
	if !ok || maxLength < 1 || maxLength > maxMealSchemaStringLength {
		return fmt.Errorf("%s: maxLength must be between 1 and %d", path, maxMealSchemaStringLength)
	}
	if rawEnum, ok := node["enum"].([]any); ok {
		if len(rawEnum) < 1 || len(rawEnum) > maxMealSchemaEnumValues {
			return fmt.Errorf("%s: enum must have 1..%d string values", path, maxMealSchemaEnumValues)
		}
		for _, value := range rawEnum {
			if _, isString := value.(string); !isString {
				return fmt.Errorf("%s: enum values must be strings", path)
			}
		}
	}
	return nil
}

func validateNumberNode(node map[string]any, path string) error {
	if !containsString(nodeTypes(node), "number") && !containsString(nodeTypes(node), "integer") {
		return fmt.Errorf("%s: expected number or integer type", path)
	}
	if minimum, ok := node["minimum"].(float64); ok && minimum < 0 {
		return fmt.Errorf("%s: minimum must be non-negative", path)
	}
	if maximum, ok := node["maximum"].(float64); ok && (maximum <= 0 || maximum > maxMealSchemaNumberMaximum) {
		return fmt.Errorf("%s: maximum must be between 0 and %d", path, maxMealSchemaNumberMaximum)
	}
	return nil
}

func nodeTypes(node map[string]any) []string {
	switch typed := node["type"].(type) {
	case string:
		return []string{typed}
	case []any:
		var types []string
		for _, value := range typed {
			if text, ok := value.(string); ok {
				types = append(types, text)
			}
		}
		return types
	default:
		return nil
	}
}

func isNullOnly(node map[string]any) bool {
	switch typed := node["type"].(type) {
	case string:
		return typed == "null" && len(node) == 1
	case []any:
		return len(node) == 1 && len(typed) == 1 && typed[0] == "null"
	default:
		return false
	}
}

func stringArray(node map[string]any, key, path string) ([]string, error) {
	raw, ok := node[key].([]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s must be an array", path, key)
	}
	values := make([]string, 0, len(raw))
	for _, value := range raw {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s: %s must contain only strings", path, key)
		}
		values = append(values, text)
	}
	return values, nil
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
