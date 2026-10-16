package albums

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAlbumResourceDeploymentsKeepStorageAndIngressIsolated(t *testing.T) {
	for _, environment := range []string{"test", "production"} {
		t.Run(environment, func(t *testing.T) { checkAlbumDeployment(t, environment) })
	}
}

func checkAlbumDeployment(t *testing.T, environment string) {
	t.Helper()
	data, err := os.ReadFile("../../compose.albums-" + environment + ".yaml")
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
	serviceCount := 5
	if environment == "production" {
		serviceCount = 4
		if _, exists := stack.Services["albumgateway"]; exists {
			t.Fatal("production Albums must use the shared API gateway")
		}
	}
	if stack.Name != "tellyouwhat-albums-"+environment || len(stack.Services) != serviceCount || len(stack.Networks) != 1 || len(stack.Volumes) != 1 {
		t.Fatal("resource services or storage are not isolated")
	}
	for name, service := range stack.Services {
		if len(service.Networks) != 1 || service.Networks[0] != "albums-"+environment {
			t.Fatal("shared service network", name)
		}
		for _, file := range service.EnvFile {
			if !strings.Contains(file, "ALBUM_"+strings.ToUpper(environment)+"_ENV_FILE") {
				t.Fatal("wrong environment file", name)
			}
		}
		if (environment == "production" || name != "albumgateway") && len(service.Ports) != 0 {
			t.Fatal("database/worker publicly exposed", name)
		}
	}
	worker := stack.Services["albumworker"]
	if worker.Environment["ALBUM_DEPLOYMENT_ENVIRONMENT"] != environment || !strings.Contains(worker.Environment["ALBUM_DATABASE_DSN"], "/albums_"+environment+"?") || len(worker.Volumes) != 0 {
		t.Fatal("unsafe worker storage environment or private-key mount")
	}
	if environment == "test" {
		api := stack.Services["albumgateway"]
		if len(api.Ports) != 1 || !strings.HasPrefix(api.Ports[0], "127.0.0.1:") || api.Environment["ALBUM_DEPLOYMENT_ENVIRONMENT"] != environment || api.Environment["ALBUM_DATABASE_DSN"] != worker.Environment["ALBUM_DATABASE_DSN"] {
			t.Fatal("unsafe test API environment")
		}
	}
	if stack.Services["mysql"].Environment["MYSQL_DATABASE"] != "albums_"+environment || len(stack.Services["mysql"].Volumes) != 1 || stack.Services["mysql"].Volumes[0] != "albums-"+environment+"-mysql:/var/lib/mysql" {
		t.Fatal("database volume not isolated")
	}
	if !strings.Contains(stack.Services["migrate"].Environment["DATABASE_DSN"], "/albums_"+environment+"?") {
		t.Fatal("migrator targets another schema")
	}
	for _, name := range []string{"albumgateway", "albumworker"} {
		for key := range stack.Services[name].Environment {
			if strings.Contains(key, "ROOT_PASSWORD") {
				t.Fatal("root password exposed to app", name)
			}
		}
	}
	runtimeName := "runtime.env.example"
	if environment == "production" {
		runtimeName = "production-runtime.env.example"
	}
	runtime, err := os.ReadFile("../../deploy/albums/" + runtimeName)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(runtime), "MYSQL_ROOT_PASSWORD=") || strings.Contains(string(runtime), "_ENV_FILE=") {
		t.Fatal("Compose administrator secrets reach application environment")
	}
}
