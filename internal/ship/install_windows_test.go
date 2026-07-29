//go:build windows

package ship

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The settings that exist purely to override a hostile default. If one of
// these disappears the supervisor regresses to a three-day life expectancy
// and no crash recovery, which no other test would notice.
func TestTaskXMLPinsTheDurabilitySettings(t *testing.T) {
	doc := taskXML(`C:\bin\shipmates.exe`, `C:\logs\ship.log`, `HOST\op`, InstallOptions{})
	for _, want := range []string{
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<StartWhenAvailable>true</StartWhenAvailable>",
		"<Count>3</Count>",
		"<Interval>PT1M</Interval>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("task XML missing %s", want)
		}
	}
}

// <Settings> is an xs:sequence: out-of-order children are rejected at
// registration with a message that says nothing about ordering. This pins
// the order against a well-meaning alphabetical tidy-up.
func TestTaskXMLSettingsOrderMatchesSchemaSequence(t *testing.T) {
	doc := taskXML(`C:\bin\shipmates.exe`, `C:\logs\ship.log`, `HOST\op`, InstallOptions{})
	order := []string{
		"<DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>",
		"<ExecutionTimeLimit>",
		"<MultipleInstancesPolicy>",
		"<RestartOnFailure>",
		"<StartWhenAvailable>",
	}
	prev := -1
	for _, el := range order {
		at := strings.Index(doc, el)
		if at < 0 {
			t.Fatalf("missing %s", el)
		}
		if at < prev {
			t.Errorf("%s is out of schema sequence order", el)
		}
		prev = at
	}
}

func TestTaskXMLLogonTypeAndTriggersFollowUnattended(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     InstallOptions
		logon    string
		wantBoot bool
	}{
		{"default is an interactive logon task", InstallOptions{}, "InteractiveToken", false},
		{"unattended defaults to S4U", InstallOptions{Unattended: true}, "S4U", true},
		{"stored password opts into a full token", InstallOptions{Unattended: true, StorePassword: true}, "Password", true},
		{"store-password without unattended stays interactive", InstallOptions{StorePassword: true}, "InteractiveToken", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := taskXML(`C:\bin\shipmates.exe`, `C:\logs\ship.log`, `HOST\op`, tc.opts)
			if want := "<LogonType>" + tc.logon + "</LogonType>"; !strings.Contains(doc, want) {
				t.Errorf("want %s", want)
			}
			if got := strings.Contains(doc, "<BootTrigger>"); got != tc.wantBoot {
				t.Errorf("BootTrigger present = %v, want %v", got, tc.wantBoot)
			}
			// The logon trigger survives in both modes; IgnoreNew keeps it
			// from starting a second supervisor on an unattended box.
			if !strings.Contains(doc, "<LogonTrigger>") {
				t.Error("LogonTrigger should be present in every mode")
			}
		})
	}
}

// A path with an ampersand is not exotic on Windows (C:\R&D\...), and an
// unescaped one produces a task XML that fails to parse.
func TestTaskXMLEscapesPaths(t *testing.T) {
	doc := taskXML(`C:\R&D\ship.exe`, `C:\R&D\ship.log`, `HOST\o'p`, InstallOptions{})
	if strings.Contains(doc, `C:\R&D\ship.exe`) {
		t.Error("ampersand in the exe path was not XML-escaped")
	}
	if !strings.Contains(doc, "C:\\R&amp;D\\ship.exe") {
		t.Error("want the escaped exe path")
	}
}

func TestUTF16LEHasBOMAndLittleEndianOrder(t *testing.T) {
	got := utf16LE("Ab")
	want := []byte{0xFF, 0xFE, 'A', 0x00, 'b', 0x00}
	if string(got) != string(want) {
		t.Errorf("utf16LE = % x, want % x", got, want)
	}
}

// Registers the generated XML against the real Task Scheduler and reads the
// settings back. This is the only check that catches a schema violation —
// the unit tests above cannot tell a valid document from a plausible one.
//
// Off by default because it mutates the machine's task store. Run with:
//
//	SHIPMATES_TASK_PROBE=1 go test ./internal/ship/ -run TaskXMLRegisters -v
func TestTaskXMLRegistersWithTaskScheduler(t *testing.T) {
	if os.Getenv("SHIPMATES_TASK_PROBE") != "1" {
		t.Skip("set SHIPMATES_TASK_PROBE=1 to register a throwaway task")
	}
	const probe = "ShipmatesXMLProbe"
	user := os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME")

	for _, tc := range []struct {
		name string
		opts InstallOptions
		args []string
	}{
		{"logon task", InstallOptions{}, nil},
		{"unattended S4U", InstallOptions{Unattended: true}, []string{"/RU", user, "/NP"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := writeTaskXML(taskXML(`C:\Windows\System32\cmd.exe`, `C:\probe.log`, user, tc.opts))
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			t.Cleanup(func() {
				_ = exec.Command("schtasks", "/Delete", "/F", "/TN", probe).Run()
			})

			args := append([]string{"/Create", "/F", "/TN", probe, "/XML", path}, tc.args...)
			if out, err := exec.Command("schtasks", args...).CombinedOutput(); err != nil {
				msg := strings.TrimSpace(string(out))
				// S4U registration needs SeBatchLogonRight, which is a
				// machine policy question rather than a defect in the XML.
				if tc.opts.Unattended && strings.Contains(strings.ToLower(msg), "logon") {
					t.Skipf("account lacks the batch-logon right for S4U: %s", msg)
				}
				t.Fatalf("schtasks rejected the generated XML: %v: %s", err, msg)
			}
			out, err := exec.Command("schtasks", "/Query", "/TN", probe, "/XML").CombinedOutput()
			if err != nil {
				t.Fatalf("query: %v: %s", err, out)
			}
			// schtasks emits UTF-16; drop the NUL bytes rather than decoding properly.
			readback := strings.ReplaceAll(string(out), "\x00", "")
			want := []string{
				"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
				"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
				"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
				"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
				"<RestartOnFailure>",
			}
			if tc.opts.Unattended {
				want = append(want, "<BootTrigger>", "<LogonType>S4U</LogonType>")
			}
			for _, w := range want {
				if !strings.Contains(readback, w) {
					t.Errorf("Windows did not retain %s\n--- readback ---\n%s", w, readback)
				}
			}
		})
	}
}
