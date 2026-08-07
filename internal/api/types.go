// Package api defines the wire types shared by captain servers, Fleet Command,
// and CLI clients. Keeping the JSON contract here prevents each transport
// layer from inventing a subtly different anonymous struct.
package api

import "time"

// Event is one item in a captain's activity timeline.
type Event struct {
	Seq        uint64  `json:"seq"`
	Time       string  `json:"time"`
	Persona    string  `json:"persona"`
	Type       string  `json:"type"`
	Text       string  `json:"text"`
	Tool       string  `json:"tool,omitempty"`
	Input      string  `json:"input,omitempty"`
	ID         string  `json:"id,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	Model      string  `json:"model,omitempty"`
}

type Captain struct {
	ClientKey string    `json:"client_key"`
	Repo      string    `json:"repo"`
	RepoURL   string    `json:"repo_url,omitempty"`
	InstallID string    `json:"install_id"`
	Persona   string    `json:"persona"`
	Port      int       `json:"port"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type CaptainStatus struct {
	Captain
	Connected bool `json:"connected"`
}

type Pending struct {
	ID      string `json:"id"`
	Persona string `json:"persona"`
	Tool    string `json:"tool"`
	Input   string `json:"input,omitempty"`
}

type FleetPending struct {
	Pending
	ClientKey string `json:"client_key"`
	Repo      string `json:"repo"`
}

type MateStatus struct {
	Persona   string `json:"persona"`
	Status    string `json:"status"`
	Since     string `json:"since,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Input     string `json:"input,omitempty"`
	PendingID string `json:"pending_id,omitempty"`
}

type FleetMateStatus struct {
	MateStatus
	ClientKey string `json:"client_key"`
	Repo      string `json:"repo"`
}
