package architecture

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"
)

func TestPublicContractsComposeOneCanonicalPlatformAPI(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	platform := loadContract(t, filepath.Join(root, "Contracts/HTTP/PlatformAPI/openapi.yaml"))
	if len(platform.Paths.Map()) != 11 {
		t.Fatalf("platform path count = %d, want 11", len(platform.Paths.Map()))
	}

	apps := []struct {
		name            string
		sourcePath      string
		publicPath      string
		appPathCount    int
		publicPathCount int
	}{
		{name: "Health", sourcePath: "Contracts/HTTP/HealthAPI/app.openapi.yaml", publicPath: "Contracts/HTTP/HealthAPI/openapi.yaml", appPathCount: 8, publicPathCount: 19},
		{name: "Journal", sourcePath: "Contracts/HTTP/JournalAPI/app.openapi.yaml", publicPath: "Contracts/HTTP/JournalAPI/openapi.yaml", appPathCount: 1, publicPathCount: 12},
	}
	for _, app := range apps {
		app := app
		t.Run(app.name, func(t *testing.T) {
			sourcePaths := loadSourcePaths(t, filepath.Join(root, app.sourcePath))
			if len(sourcePaths) != app.appPathCount {
				t.Fatalf("App-specific path count = %d, want %d", len(sourcePaths), app.appPathCount)
			}
			for path := range sourcePaths {
				if platform.Paths.Find(path) != nil {
					t.Fatalf("App source duplicates platform path %s", path)
				}
			}

			public := loadContract(t, filepath.Join(root, app.publicPath))
			if len(public.Paths.Map()) != app.publicPathCount {
				t.Fatalf("public path count = %d, want %d", len(public.Paths.Map()), app.publicPathCount)
			}
			operationIDs := make(map[string]string)
			for path, item := range public.Paths.Map() {
				isPlatformPath := platform.Paths.Find(path) != nil
				for method, operation := range item.Operations() {
					location := strings.ToUpper(method) + " " + path
					if previous, exists := operationIDs[operation.OperationID]; exists {
						t.Fatalf("operationId %q is shared by %s and %s", operation.OperationID, previous, location)
					}
					operationIDs[operation.OperationID] = location
					if taggedPlatform := contains(operation.Tags, "Platform"); taggedPlatform != isPlatformPath {
						t.Fatalf("%s Platform tag = %t, want %t", location, taggedPlatform, isPlatformPath)
					}
				}
			}
			for path, platformItem := range platform.Paths.Map() {
				publicItem := public.Paths.Find(path)
				if publicItem == nil {
					t.Fatalf("public contract is missing platform path %s", path)
				}
				for method, platformOperation := range platformItem.Operations() {
					publicOperation := publicItem.GetOperation(method)
					if publicOperation == nil || publicOperation.OperationID != platformOperation.OperationID {
						t.Fatalf("%s %s does not preserve platform operation %q", method, path, platformOperation.OperationID)
					}
				}
			}
		})
	}

	for name, scheme := range platform.Components.SecuritySchemes {
		if scheme.Value == nil {
			t.Fatalf("security scheme %s is unresolved", name)
		}
		if strings.HasPrefix(strings.ToLower(scheme.Value.Name), "x-health-") {
			t.Fatalf("platform security scheme %s retains product-specific header %s", name, scheme.Value.Name)
		}
	}
	for name, parameter := range platform.Components.Parameters {
		if parameter.Value == nil {
			t.Fatalf("parameter %s is unresolved", name)
		}
		if parameter.Value.In == "header" && strings.HasPrefix(strings.ToLower(parameter.Value.Name), "x-health-") {
			t.Fatalf("platform parameter %s retains product-specific header %s", name, parameter.Value.Name)
		}
	}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func loadContract(t *testing.T, path string) *openapi3.T {
	t.Helper()
	document, err := openapi3.NewLoader().LoadFromFile(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate %s: %v", path, err)
	}
	return document
}

func loadSourcePaths(t *testing.T, path string) map[string]any {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(source, &document); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no paths map", path)
	}
	return paths
}
