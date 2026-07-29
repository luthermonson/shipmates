//go:build windows

package ship

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// taskName is the Scheduled Task the supervisor runs under.
const taskName = "ShipmatesShip"

// Install registers the supervisor as a Scheduled Task in the operator's own
// account rather than as a session-0 service. The runtimes shipmates drives
// read credentials and state from the user's profile, which a LocalSystem
// service does not have.
//
// The task is defined in XML rather than through `schtasks /Create` flags.
// That is not a stylistic preference: ExecutionTimeLimit, the battery
// policy, RestartOnFailure and the boot trigger are all unreachable from the
// command-line switches, and their defaults are actively wrong for a
// long-lived supervisor — a schtasks-created task is killed after 72 hours,
// is stopped the moment a UPS takes over, and is never restarted if it dies.
func Install(opts InstallOptions) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(home, ".shipmates", "ship.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolving current user: %w", err)
	}

	xmlPath, err := writeTaskXML(taskXML(exe, logPath, u.Username, opts))
	if err != nil {
		return err
	}
	defer os.Remove(xmlPath)

	args := []string{"/Create", "/F", "/TN", taskName, "/XML", xmlPath}
	if opts.Unattended {
		// The XML carries the logon type; schtasks still wants the account
		// named on the command line so it can attach (or deliberately not
		// attach) a credential to it.
		args = append(args, "/RU", u.Username)
		if opts.StorePassword {
			// Bare /RP: schtasks prompts on the console. The password never
			// enters our address space or a command line.
			args = append(args, "/RP")
		} else {
			args = append(args, "/NP")
		}
	}

	create := exec.Command("schtasks", args...)
	if opts.Unattended && opts.StorePassword {
		// schtasks owns the terminal for the duration of its prompt.
		create.Stdin, create.Stdout, create.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := create.Run(); err != nil {
			return fmt.Errorf("schtasks create: %w", err)
		}
	} else if out, err := create.CombinedOutput(); err != nil {
		msg := fmt.Errorf("schtasks create: %v: %s", err, strings.TrimSpace(string(out)))
		if opts.Unattended {
			// Registering an S4U task needs the "Log on as a batch job"
			// right, which a plain user account often lacks. Say so rather
			// than leaving the operator with a bare exit code.
			return fmt.Errorf("%w\n\nan unattended install needs the 'Log on as a batch job' right for %s; "+
				"either grant it (secpol.msc → Local Policies → User Rights Assignment), "+
				"run this from an elevated prompt, or retry with --store-password", msg, u.Username)
		}
		return msg
	}

	// Start it now — both triggers are in the future otherwise.
	if out, err := exec.Command("schtasks", "/Run", "/TN", taskName).CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall stops and removes the Scheduled Task.
func Uninstall() error {
	_ = exec.Command("schtasks", "/End", "/TN", taskName).Run() // best-effort stop
	if out, err := exec.Command("schtasks", "/Delete", "/F", "/TN", taskName).CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks delete: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// logonType maps the install options onto a Task Scheduler principal.
//
// InteractiveToken needs a live desktop session; the other two do not, which
// is the whole point of an unattended install. S4U gets there without
// storing a secret, at the cost of a token with no network credentials.
func logonType(opts InstallOptions) string {
	switch {
	case !opts.Unattended:
		return "InteractiveToken"
	case opts.StorePassword:
		return "Password"
	default:
		return "S4U"
	}
}

// taskXML renders the task definition.
//
// Element order inside <Settings> is load-bearing: the schema declares it as
// a sequence, and a block in the wrong order is rejected with "The task XML
// contains a value which is incorrectly formatted or out of range" — a
// message that says nothing about ordering. The order below is the one
// `schtasks /Query /XML` emits for a system task, which is the only
// authority worth trusting here.
//
// Every setting present is present because its default is wrong for a
// supervisor:
//
//	DisallowStartIfOnBatteries/StopIfGoingOnBatteries  default true — a UPS
//	    taking over during an outage would stop the ship and refuse to
//	    restart it, making UPS-backed hardware fail *more* often.
//	ExecutionTimeLimit  defaults to PT72H; PT0S means no limit. Without it
//	    the supervisor is terminated after three days, silently.
//	MultipleInstancesPolicy  IgnoreNew so the boot and logon triggers cannot
//	    race into two supervisors.
//	RestartOnFailure  the Windows equivalent of launchd KeepAlive. Without
//	    it a crash is terminal until the next boot.
//	StartWhenAvailable  runs a missed trigger late rather than never.
func taskXML(exe, logPath, userID string, opts InstallOptions) string {
	var b strings.Builder
	esc := func(s string) string {
		var buf bytes.Buffer
		_ = xml.EscapeText(&buf, []byte(s))
		return buf.String()
	}

	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>` + "\n")
	b.WriteString(`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\n")
	b.WriteString("  <RegistrationInfo>\n")
	b.WriteString("    <Description>shipmates ship supervisor</Description>\n")
	b.WriteString("    <URI>\\" + esc(taskName) + "</URI>\n")
	b.WriteString("  </RegistrationInfo>\n")

	b.WriteString("  <Principals>\n")
	b.WriteString(`    <Principal id="Author">` + "\n")
	b.WriteString("      <UserId>" + esc(userID) + "</UserId>\n")
	b.WriteString("      <LogonType>" + logonType(opts) + "</LogonType>\n")
	b.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\n")
	b.WriteString("    </Principal>\n")
	b.WriteString("  </Principals>\n")

	b.WriteString("  <Settings>\n")
	b.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	b.WriteString("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\n")
	b.WriteString("    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>\n")
	b.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n")
	b.WriteString("    <RestartOnFailure>\n")
	b.WriteString("      <Count>3</Count>\n")
	b.WriteString("      <Interval>PT1M</Interval>\n")
	b.WriteString("    </RestartOnFailure>\n")
	b.WriteString("    <StartWhenAvailable>true</StartWhenAvailable>\n")
	b.WriteString("  </Settings>\n")

	b.WriteString("  <Triggers>\n")
	if opts.Unattended {
		// 30s of slack so the supervisor is not racing the network stack
		// on the way up.
		b.WriteString("    <BootTrigger>\n")
		b.WriteString("      <Enabled>true</Enabled>\n")
		b.WriteString("      <Delay>PT30S</Delay>\n")
		b.WriteString("    </BootTrigger>\n")
	}
	// Kept in both modes: on an unattended box the supervisor is already
	// running by the time anyone logs in, and IgnoreNew makes this a no-op.
	b.WriteString("    <LogonTrigger>\n")
	b.WriteString("      <Enabled>true</Enabled>\n")
	b.WriteString("      <UserId>" + esc(userID) + "</UserId>\n")
	b.WriteString("    </LogonTrigger>\n")
	b.WriteString("  </Triggers>\n")

	b.WriteString(`  <Actions Context="Author">` + "\n")
	b.WriteString("    <Exec>\n")
	b.WriteString("      <Command>" + esc(exe) + "</Command>\n")
	b.WriteString("      <Arguments>" + esc(`ship serve --log-file "`+logPath+`"`) + "</Arguments>\n")
	b.WriteString("    </Exec>\n")
	b.WriteString("  </Actions>\n")
	b.WriteString("</Task>\n")
	return b.String()
}

// writeTaskXML spills the definition to a temp file for `schtasks /XML`.
//
// The file is UTF-16LE with a BOM. schtasks rejects UTF-8 task XML on some
// builds with the same unhelpful "incorrectly formatted" error as a schema
// violation, so this is not optional.
func writeTaskXML(doc string) (string, error) {
	f, err := os.CreateTemp("", "shipmates-task-*.xml")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(utf16LE(doc)); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// utf16LE encodes s as little-endian UTF-16 with a byte-order mark.
func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(units)*2+2)
	b = append(b, 0xFF, 0xFE) // BOM
	for _, u := range units {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}
