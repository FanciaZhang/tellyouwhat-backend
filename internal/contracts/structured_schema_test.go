package contracts

import (
	"errors"
	"strings"
	"testing"
)

func TestStructuredMealSchemaAcceptsRegisteredShapes(t *testing.T) {
	t.Parallel()

	if err := ValidateStructuredMealSchema([]byte(validHydrationSchemaJSON()), OperationMealPhotoCapture); err != nil {
		t.Fatalf("hydration schema rejected: %v", err)
	}
	for _, operation := range []Operation{OperationMealPhotoCapture, OperationMealTextCapture} {
		if err := ValidateStructuredMealSchema([]byte(validV5MealSchemaJSON()), operation); err != nil {
			t.Fatalf("v5 meal schema rejected for %s: %v", operation, err)
		}
		if err := ValidateStructuredMealSchema([]byte(validV5SpecifiedFoodSchemaJSON()), operation); err != nil {
			t.Fatalf("extended specified-food schema rejected for %s: %v", operation, err)
		}
	}
}

func TestStructuredMealSchemaRejectsExternalV5RootField(t *testing.T) {
	t.Parallel()
	raw := strings.Replace(validV5MealSchemaJSON(),
		`"water_candidates":{"type":"array","maxItems":6,"items":{"type":"string","maxLength":1024}}`,
		`"water_candidates":{"type":"array","maxItems":6,"items":{"type":"string","maxLength":1024}},"components":{"type":"array","maxItems":1,"items":{"type":"string","maxLength":16}}`,
		1)
	if err := ValidateStructuredMealSchema([]byte(raw), OperationMealPhotoCapture); err == nil {
		t.Fatal("external v5 root field must be rejected")
	}
}

func TestStructuredMealSchemaRejectsUnknownNutritionField(t *testing.T) {
	t.Parallel()

	raw := strings.Replace(
		validV5SpecifiedFoodSchemaJSON(),
		`"required":["protein"]`,
		`"required":["protein","invented"]`,
		1,
	)
	raw = strings.Replace(
		raw,
		`"properties":{"protein":{"type":"number","minimum":0}}`,
		`"properties":{"protein":{"type":"number","minimum":0},"invented":{"type":"number","minimum":0}}`,
		1,
	)
	if err := ValidateStructuredMealSchema([]byte(raw), OperationMealPhotoCapture); err == nil {
		t.Fatal("unknown nutrition field must be rejected")
	}
}

func TestStructuredMealSchemaRejectsDangerousSchemas(t *testing.T) {
	t.Parallel()

	base := validHydrationSchemaJSON()
	cases := map[string]string{
		"remote reference": strings.Replace(base,
			`"recognized":{"type":"boolean"}`,
			`"recognized":{"$ref":"https://evil.example/schema"}`,
			1),
		"unbounded string": strings.Replace(base,
			`"basis":{"type":"string","maxLength":1024}`,
			`"basis":{"type":"string"}`,
			1),
		"unbounded array": strings.Replace(base,
			`"confidence":{"type":"string","maxLength":64,"enum":["low","medium","high"]}`,
			`"confidence":{"type":"array","items":{"type":"string","maxLength":64},"maxItems":100}`,
			1),
		"negative minimum": strings.Replace(base,
			`"capacity_ml":{"type":"number","minimum":50,"maximum":3000}`,
			`"capacity_ml":{"type":"number","minimum":-5,"maximum":3000}`,
			1),
		"missing root key": strings.Replace(base,
			`"basis":{"type":"string","maxLength":1024}`,
			`"other":{"type":"string","maxLength":1024}`,
			1),
	}
	for name, raw := range cases {
		if err := ValidateStructuredMealSchema([]byte(raw), OperationMealPhotoCapture); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

func TestManifestValidatesStructuredMealSchema(t *testing.T) {
	t.Parallel()

	manifest, err := ParseManifest([]byte(testManifestJSON()))
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		Operation:       OperationMealPhotoCapture,
		ContractVersion: ContractVersionV1,
		PromptVersion:   "meal-photo-v5",
		ResponseSchema:  []byte(validHydrationSchemaJSON()),
	}
	if err := manifest.Validate(request); err != nil {
		t.Fatalf("structured hydration schema rejected: %v", err)
	}

	invalidNutrition := strings.Replace(
		validV5SpecifiedFoodSchemaJSON(),
		`"required":["protein"]`,
		`"required":["protein","invented"]`,
		1,
	)
	invalidNutrition = strings.Replace(
		invalidNutrition,
		`"properties":{"protein":{"type":"number","minimum":0}}`,
		`"properties":{"protein":{"type":"number","minimum":0},"invented":{"type":"number","minimum":0}}`,
		1,
	)
	request.ResponseSchema = []byte(invalidNutrition)
	if err := manifest.Validate(request); !errors.Is(err, ErrContractViolation) {
		t.Fatalf("unknown nutrition field must be a contract violation, got %v", err)
	}
}

func TestStructuredManifestEntryRequiresEmptyDigests(t *testing.T) {
	t.Parallel()

	raw := `{"entries":[{"operation":"meal_photo_capture","contractVersion":"ai-request-v1","promptVersion":"meal-photo-v5","schemaPolicy":"structured","schemaSHA256":["cd1a463c46d6264134447db17a8c3c7abe5b9a2488c6d759fea66da1f96b133e"],"maxTemperature":1,"allowedReasoningEfforts":[""],"allowStream":true,"allowWebSearch":false}]}`
	if _, err := ParseManifest([]byte(raw)); err == nil {
		t.Fatal("structured entry with digests must be rejected")
	}
}

func validV5SpecifiedFoodSchemaJSON() string {
	return `{
		"type":"object",
		"additionalProperties":false,
		"required":["food_id","name","grams","kcal","nutrition","image_index","box","part_analysis","recipe_variants","default_recipe_variant_id","food_context"],
		"properties":{
			"food_id":{"type":"string","maxLength":1024},
			"name":{"type":"string","maxLength":1024},
			"grams":{"type":"number","minimum":0.000001},
			"kcal":{"type":"number","minimum":0},
			"nutrition":{
				"type":"object",
				"additionalProperties":false,
				"required":["protein"],
				"properties":{"protein":{"type":"number","minimum":0}}
			},
			"image_index":{"anyOf":[{"type":"integer","minimum":1,"maximum":12},{"type":"null"}]},
			"box":{"anyOf":[{"type":"array","minItems":4,"maxItems":4,"items":{"type":"number","minimum":0,"maximum":1}},{"type":"null"}]},
			"part_analysis":{"type":"object","additionalProperties":false,"required":["status","edible_parts"],"properties":{"status":{"type":"string","maxLength":32,"enum":["generated","not_applicable","insufficient_evidence"]},"edible_parts":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":1024}}}},
			"recipe_variants":{"type":"array","maxItems":4,"items":{"type":"string","maxLength":1024}},
			"default_recipe_variant_id":{"anyOf":[{"type":"string","maxLength":1024},{"type":"null"}]},
			"food_context":{"type":"object","additionalProperties":false,"required":["meal_role"],"properties":{"meal_role":{"type":"string","maxLength":64,"enum":["primary"]}}}
		}
	}`
}

func validV5MealSchemaJSON() string {
	return `{
		"type":"object",
		"additionalProperties":false,
		"required":["foods","meal_context","water_candidates"],
		"properties":{
			"foods":{"type":"array","maxItems":12,"items":{"type":"string","maxLength":1024}},
			"meal_context":{"type":"object","additionalProperties":false,"required":["dining_scene"],"properties":{"dining_scene":{"type":"string","maxLength":64,"enum":["unknown"]}}},
			"water_candidates":{"type":"array","maxItems":6,"items":{"type":"string","maxLength":1024}}
		}
	}`
}

func validHydrationSchemaJSON() string {
	return `{
		"type":"object",
		"additionalProperties":false,
		"required":["recognized","suggested_name","capacity_ml","lower_bound_ml","upper_bound_ml","confidence","basis"],
		"properties":{
			"recognized":{"type":"boolean"},
			"suggested_name":{"type":"string","maxLength":1024},
			"capacity_ml":{"type":"number","minimum":50,"maximum":3000},
			"lower_bound_ml":{"type":"number","minimum":50,"maximum":3000},
			"upper_bound_ml":{"type":"number","minimum":50,"maximum":3000},
			"confidence":{"type":"string","maxLength":64,"enum":["low","medium","high"]},
			"basis":{"type":"string","maxLength":1024}
		}
	}`
}
