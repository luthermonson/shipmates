package bridge

import "testing"

func TestRescueTextToolCalls(t *testing.T) {
	content := "Tell_lead will wake up the leads. Starting with \"homelab:lead\".\n\n" +
		"Tell_lead {\"lead_key\": \"homelab:lead\", \"persona\": \"lead\", \"message\": \"/standup\"}.\n" +
		"tell_lead {\"lead_key\": \"laptop:lead\", \"persona\": \"lead\", \"message\": \"/standup\"}\n" +
		"not_a_tool {\"x\": 1}\n" +
		"fleet_status {broken json}\n"
	got := rescueTextToolCalls(content)
	if len(got) != 2 {
		t.Fatalf("want 2 rescued calls, got %d: %+v", len(got), got)
	}
	for i, tc := range got {
		if tc.Function.Name != "tell_lead" {
			t.Errorf("call %d name = %q", i, tc.Function.Name)
		}
		if tc.Args()["lead_key"] == "" {
			t.Errorf("call %d lost its arguments", i)
		}
	}
}

func TestRescueIgnoresProse(t *testing.T) {
	for _, content := range []string{
		"All ships are online and idle.",
		"The bead {proj-59m} is assigned already.", // braces but no valid call
		"",
	} {
		if got := rescueTextToolCalls(content); len(got) != 0 {
			t.Fatalf("prose %q rescued %+v", content, got)
		}
	}
}
