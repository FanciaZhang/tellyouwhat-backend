package platformops

import "testing"

func TestValidateHostChecks(t *testing.T) {
	legacy := []HostCheck{}
	for _, name := range []string{"health_gateway", "journal_gateway", "worker", "admin", "disk_space", "backup_freshness", "maintenance_freshness", "restore_freshness"} {
		legacy = append(legacy, HostCheck{Name: name, Passed: true})
	}
	managed := append(append([]HostCheck{}, legacy...), HostCheck{Name: "deployment_state", Passed: true}, HostCheck{Name: "deployment_controller", Passed: false})
	birthday := append(append([]HostCheck{}, legacy...), HostCheck{Name: "birthday_gateway", Passed: false})
	birthdayManaged := append(append([]HostCheck{}, managed...), HostCheck{Name: "birthday_gateway", Passed: true})
	birthdayDuplicate := append(append([]HostCheck{}, birthday...), HostCheck{Name: "birthday_gateway", Passed: true})
	birthdayUnpaired := append(append([]HostCheck{}, birthday...), HostCheck{Name: "deployment_state", Passed: true})
	duplicate := append([]HostCheck{}, managed...)
	duplicate[9].Name = "deployment_state"
	unknown := append([]HostCheck{}, managed...)
	unknown[9].Name = "unknown"
	for _, tc := range []struct {
		name   string
		checks []HostCheck
		valid  bool
	}{
		{"birthday failed", birthday, true}, {"birthday managed", birthdayManaged, true},
		{"birthday duplicate", birthdayDuplicate, false}, {"birthday missing lifecycle pair", birthdayUnpaired, false},
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
