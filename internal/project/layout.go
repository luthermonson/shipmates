package project

import "path/filepath"

const (
	Dir             = ".shipmates"
	MemoryDirName   = "memory"
	SessionsDirName = "sessions"
	PoliciesDirName = "policies"
	ManifestName    = "manifest.json"
	ConfigName      = "shipmates.yaml"
	InstallIDName   = "install-id"
	AgentsDir       = ".claude/agents"
	CodexAgentsDir  = ".codex/agents"
	CommandsDir     = ".claude/commands"
)

func MemoryDir(persona string) string { return filepath.Join(Dir, MemoryDirName, persona) }
func AgentPath(persona string) string { return filepath.Join(AgentsDir, persona+".md") }
func CodexAgentPath(persona string) string {
	return filepath.Join(CodexAgentsDir, persona+".toml")
}
func CommandPath(name string) string { return filepath.Join(CommandsDir, name+".md") }
func PoliciesDir() string            { return filepath.Join(Dir, PoliciesDirName) }
func PolicyPath(persona string) string {
	return filepath.Join(PoliciesDir(), persona+".yaml")
}
func ManifestPath() string  { return filepath.Join(Dir, ManifestName) }
func SessionsDir() string   { return filepath.Join(Dir, SessionsDirName) }
func PortFile() string      { return filepath.Join(SessionsDir(), "server.port") }
func PidFile() string       { return filepath.Join(SessionsDir(), "server.pid") }
func LogFile() string       { return filepath.Join(SessionsDir(), "server.log") }
func InstallIDPath() string { return filepath.Join(Dir, InstallIDName) }
