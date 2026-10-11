package albums

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAlbumTestDeploymentHasIndependentStorageAndOnlyLoopbackIngress(t *testing.T) {
	data, err := os.ReadFile("../../compose.albums-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var stack struct {
		Name     string `yaml:"name"`
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			EnvFile     []string          `yaml:"env_file"`
			Ports       []string          `yaml:"ports"`
			Networks    []string          `yaml:"networks"`
			Volumes     []string          `yaml:"volumes"`
		} `yaml:"services"`
		Networks map[string]map[string]any `yaml:"networks"`
		Volumes  map[string]any            `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(data, &stack); err != nil {
		t.Fatal(err)
	}
	if stack.Name != "tellyouwhat-albums-test" || len(stack.Services) != 5 || len(stack.Networks) != 1 || len(stack.Volumes) != 1 {
		t.Fatal("stack is not standalone")
	}
	for name, service := range stack.Services {
		if len(service.Networks) != 1 || service.Networks[0] != "albums-test" {
			t.Fatal("shared service network", name)
		}
		for _, file := range service.EnvFile {
			if strings.Contains(file, "production") {
				t.Fatal("production env reused", name)
			}
		}
		if name != "albumgateway" && len(service.Ports) != 0 {
			t.Fatal("database/worker publicly exposed", name)
		}
	}
	api := stack.Services["albumgateway"]
	if len(api.Ports) != 1 || !strings.HasPrefix(api.Ports[0], "127.0.0.1:") || api.Environment["ALBUM_DEPLOYMENT_ENVIRONMENT"] != "test" || !strings.Contains(api.Environment["ALBUM_DATABASE_DSN"], "/albums_test?") {
		t.Fatal("unsafe API environment")
	}
	if stack.Services["mysql"].Environment["MYSQL_DATABASE"] != "albums_test" || len(stack.Services["mysql"].Volumes) != 1 || stack.Services["mysql"].Volumes[0] != "albums-test-mysql:/var/lib/mysql" {
		t.Fatal("database volume not isolated")
	}
	if !strings.Contains(stack.Services["migrate"].Environment["DATABASE_DSN"], "/albums_test?") {
		t.Fatal("migrator targets another schema")
	}
	runtime, err := os.ReadFile("../../deploy/albums/runtime.env.example")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(runtime), "ALBUM_TEST_MYSQL_ROOT_PASSWORD=") || strings.Contains(string(runtime), "ALBUM_TEST_ENV_FILE=") {
		t.Fatal("Compose administrator secrets reach application environment")
	}
}
