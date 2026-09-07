package permission

import "testing"

type fixedApprover bool

func (f fixedApprover) Approve(string, map[string]any) bool { return bool(f) }

func TestPolicies(t *testing.T) {
	if decision := Check(Never, false, "read_file", nil, nil); !decision.Allowed {
		t.Fatal("read-only tool should not require approval")
	}
	if decision := Check(Auto, true, "run_shell", nil, nil); !decision.Allowed {
		t.Fatal("auto policy denied risky tool")
	}
	if decision := Check(Never, true, "run_shell", nil, nil); decision.Allowed || decision.SecurityEvent != "approval_denied" {
		t.Fatalf("never decision=%#v", decision)
	}
	if decision := Check(Ask, true, "run_shell", nil, fixedApprover(true)); !decision.Allowed {
		t.Fatal("explicit approval was ignored")
	}
	if decision := Check(Policy("invalid"), true, "run_shell", nil, nil); decision.Allowed {
		t.Fatal("invalid policy failed open")
	}
}
