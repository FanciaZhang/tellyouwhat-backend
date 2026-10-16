package albums

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAlbumDeploymentsHaveIndependentStorageAndOnlyLoopbackIngress(t *testing.T) {
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
	if stack.Name != "tellyouwhat-albums-"+environment || len(stack.Services) != 5 || len(stack.Networks) != 1 || len(stack.Volumes) != 1 {
		t.Fatal("stack is not standalone")
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
		if name != "albumgateway" && len(service.Ports) != 0 {
			t.Fatal("database/worker publicly exposed", name)
		}
	}
	api := stack.Services["albumgateway"]
	if len(api.Ports) != 1 || !strings.HasPrefix(api.Ports[0], "127.0.0.1:") || api.Environment["ALBUM_DEPLOYMENT_ENVIRONMENT"] != environment || !strings.Contains(api.Environment["ALBUM_DATABASE_DSN"], "/albums_"+environment+"?") {
		t.Fatal("unsafe API environment")
	}
	worker := stack.Services["albumworker"]
	if worker.Environment["ALBUM_DEPLOYMENT_ENVIRONMENT"] != api.Environment["ALBUM_DEPLOYMENT_ENVIRONMENT"] || worker.Environment["ALBUM_DATABASE_DSN"] != api.Environment["ALBUM_DATABASE_DSN"] {
		t.Fatal("worker and API storage environment differ")
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
