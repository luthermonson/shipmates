package fleet

import "testing"

func TestRescueTextToolCalls(t *testing.T) {
	content := "Tell_captain will wake up the captains. Starting with \"homelab:captain\".\n\n" +
		"Tell_captain {\"captain_key\": \"homelab:captain\", \"persona\": \"captain\", \"message\": \"/standup\"}.\n" +
		"tell_captain {\"captain_key\": \"laptop:captain\", \"persona\": \"captain\", \"message\": \"/standup\"}\n" +
		"not_a_tool {\"x\": 1}\n" +
		"fleet_status {broken json}\n"
	got := rescueTextToolCalls(content)
	if len(got) != 2 {
		t.Fatalf("want 2 rescued calls, got %d: %+v", len(got), got)
	}
	for i, tc := range got {
		if tc.Function.Name != "tell_captain" {
			t.Errorf("call %d name = %q", i, tc.Function.Name)
		}
		if tc.Args()["captain_key"] == "" {
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
