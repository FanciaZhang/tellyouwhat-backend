package openapicompose

import (
	"strings"
	"testing"
)

const platformFixture = `openapi: 3.1.0
info: {title: Platform, version: 1.0.0}
tags: [{name: Platform}]
paths:
  /healthz:
    get:
      operationId: getHealth
      tags: [Platform]
      responses:
        "200": {description: ok}
components:
  schemas:
    Status: {type: object}
`

const appFixture = `openapi: 3.1.0
info: {title: App, version: 1.0.0}
tags: [{name: App}]
paths:
  /v1/app:
    post:
      operationId: runApp
      tags: [App]
      responses:
        "204": {description: done}
components:
  schemas:
    AppRequest:
      type: object
      properties:
        zeta: {type: string}
        alpha: {type: string}
`

func TestComposeMergesPathsComponentsAndTagsDeterministically(t *testing.T) {
	t.Parallel()
	first, err := Compose([]byte(platformFixture), []byte(appFixture))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compose([]byte(platformFixture), []byte(appFixture))
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(first, second) {
		t.Fatal("composition is not deterministic")
	}
	for _, expected := range []string{"/healthz", "/v1/app", "Status:", "AppRequest:", "name: Platform", "name: App"} {
		if !strings.Contains(string(first), expected) {
			t.Fatalf("composed contract is missing %q", expected)
		}
	}
	output := string(first)
	if strings.Index(output, "        zeta:") > strings.Index(output, "        alpha:") {
		t.Fatal("composition changed schema property order")
	}
}

func TestComposeRejectsPathAndComponentCollisions(t *testing.T) {
	t.Parallel()
	for _, app := range []string{
		strings.Replace(appFixture, "/v1/app", "/healthz", 1),
		strings.Replace(appFixture, "AppRequest", "Status", 1),
	} {
		if _, err := Compose([]byte(platformFixture), []byte(app)); err == nil {
			t.Fatal("collision was accepted")
		}
	}
}
