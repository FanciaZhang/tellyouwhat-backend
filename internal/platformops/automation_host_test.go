package platformops

import "testing"

func TestValidateHostChecks(t *testing.T) {
	legacy := []HostCheck{}
	for _, name := range []string{"health_gateway", "journal_gateway", "worker", "admin", "disk_space", "backup_freshness", "maintenance_freshness", "restore_freshness"} {
		legacy = append(legacy, HostCheck{Name: name, Passed: true})
	}
	managed := append(append([]HostCheck{}, legacy...), HostCheck{Name: "deployment_state", Passed: true}, HostCheck{Name: "deployment_controller", Passed: false})
	duplicate := append([]HostCheck{}, managed...)
	duplicate[9].Name = "deployment_state"
	unknown := append([]HostCheck{}, managed...)
	unknown[9].Name = "unknown"
	for _, tc := range []struct {
		name   string
		checks []HostCheck
		valid  bool
	}{
		{"legacy", legacy, true}, {"managed with failed controller", managed, true},
		{"missing lifecycle pair", managed[:9], false}, {"duplicate", duplicate, false},
		{"unknown", unknown, false}, {"missing base check", legacy[:7], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateHostChecks(tc.checks); (err == nil) != tc.valid {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}
