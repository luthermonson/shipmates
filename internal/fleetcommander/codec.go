// Package fleetcommander contains the closed, transport-neutral M3 mailbox
// vocabulary. It has no socket, command, UI, or execution dependency.
package fleetcommander

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	MessageSchema       = "fleet.commander.mailbox-message.v1"
	CarrierSchema       = "fleet.commander.mailbox-carrier.v1"
	InstructionType     = "commander.instruction.v1"
	ProgressType        = "captain.progress.v1"
	CompletedType       = "captain.completed.v1"
	MaxMessageBytes     = 32 << 10
	MaxEnvelopeBytes    = 12 << 10
	MailboxDigestDomain = `shipmates/fleet-commander/m3/mailbox-message-digest\0`
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{15,95}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var timePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$`)

type Direction string

const (
	FleetToShip Direction = "fleet_to_ship"
	ShipToFleet Direction = "ship_to_fleet"
)

type Instruction struct {
	Type           string          `json:"type"`
	EnvelopeDigest string          `json:"envelope_digest"`
	Envelope       json.RawMessage `json:"envelope"`
}
type Progress struct {
	Type         string `json:"type"`
	DelegationID string `json:"delegation_id"`
	State        string `json:"state"`
}
type Completed struct {
	Type             string `json:"type"`
	DelegationID     string `json:"delegation_id"`
	Result           string `json:"result"`
	ReasonCode       string `json:"reason_code"`
	AdvisoryDecision string `json:"advisory_decision,omitempty"`
	ProvenanceDigest string `json:"provenance_digest"`
	SailState        string `json:"sail_state"`
}

type Message struct {
	Schema          string          `json:"schema"`
	MessageID       string          `json:"message_id"`
	InstructionID   string          `json:"instruction_id"`
	FleetID         string          `json:"fleet_id"`
	ShipID          string          `json:"ship_id"`
	Direction       Direction       `json:"direction"`
	MailboxSequence uint64          `json:"mailbox_sequence"`
	ExpiresAt       time.Time       `json:"expires_at"`
	Body            json.RawMessage `json:"body"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	type wire struct {
		Schema          string          `json:"schema"`
		MessageID       string          `json:"message_id"`
		InstructionID   string          `json:"instruction_id"`
		FleetID         string          `json:"fleet_id"`
		ShipID          string          `json:"ship_id"`
		Direction       Direction       `json:"direction"`
		MailboxSequence uint64          `json:"mailbox_sequence"`
		ExpiresAt       string          `json:"expires_at"`
		Body            json.RawMessage `json:"body"`
	}
	return json.Marshal(wire{m.Schema, m.MessageID, m.InstructionID, m.FleetID, m.ShipID, m.Direction, m.MailboxSequence, m.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z"), m.Body})
}

type Carrier struct {
	Schema               string   `json:"schema"`
	FleetID              string   `json:"fleet_id"`
	ShipID               string   `json:"ship_id"`
	ConnectionGeneration uint64   `json:"connection_generation"`
	Type                 string   `json:"type"`
	FleetToShipAck       uint64   `json:"fleet_to_ship_ack"`
	ShipToFleetAck       uint64   `json:"ship_to_fleet_ack"`
	Message              *Message `json:"message,omitempty"`
}

func validateID(s string) bool     { return idPattern.MatchString(s) }
func validateDigest(s string) bool { return digestPattern.MatchString(s) }
func validateTime(t time.Time) bool {
	return t.UTC().Nanosecond()%1_000_000 == 0
}

// DecodeMessage rejects duplicate keys, unknown fields, trailing JSON, and
// all schema combinations not permitted by M3 before returning typed data.
func DecodeMessage(raw []byte) (Message, error) {
	if len(raw) == 0 || len(raw) > MaxMessageBytes {
		return Message{}, errors.New("mailbox message exceeds bound")
	}
	value, err := strictValue(raw)
	if err != nil {
		return Message{}, err
	}
	obj, ok := value.(map[string]any)
	timestamp, timestampOK := obj["expires_at"].(string)
	if !ok || !timestampOK || !timePattern.MatchString(timestamp) {
		return Message{}, errors.New("invalid mailbox timestamp")
	}
	var m Message
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil || dec.More() {
		return Message{}, errors.New("invalid mailbox message")
	}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	return m, nil
}

func (m Message) Validate() error {
	if m.Schema != MessageSchema || !validateID(m.MessageID) || !validateID(m.InstructionID) || !validateID(m.FleetID) || !validateID(m.ShipID) || m.MailboxSequence == 0 || !validateTime(m.ExpiresAt) || len(m.Body) == 0 {
		return errors.New("invalid mailbox message")
	}
	if m.Direction != FleetToShip && m.Direction != ShipToFleet {
		return errors.New("invalid mailbox direction")
	}
	value, err := strictValue(m.Body)
	if err != nil {
		return err
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return errors.New("mailbox body must be object")
	}
	t, _ := obj["type"].(string)
	switch t {
	case InstructionType:
		if m.Direction != FleetToShip || len(obj) != 3 {
			return errors.New("invalid instruction direction or fields")
		}
		var b Instruction
		if err := decodeClosed(m.Body, &b); err != nil {
			return err
		}
		if !validateDigest(b.EnvelopeDigest) || len(b.Envelope) == 0 || len(b.Envelope) > MaxEnvelopeBytes {
			return errors.New("invalid instruction envelope")
		}
		digest, err := envelopeDigest(b.Envelope)
		if err != nil || digest != b.EnvelopeDigest {
			return errors.New("instruction envelope digest mismatch")
		}
	case ProgressType:
		if m.Direction != ShipToFleet || len(obj) != 3 {
			return errors.New("invalid progress direction or fields")
		}
		var b Progress
		if err := decodeClosed(m.Body, &b); err != nil {
			return err
		}
		if !validateID(b.DelegationID) || (b.State != "received" && b.State != "accepted" && b.State != "assessing") {
			return errors.New("invalid progress")
		}
	case CompletedType:
		if m.Direction != ShipToFleet || len(obj) < 5 || len(obj) > 7 {
			return errors.New("invalid completed direction or fields")
		}
		var b Completed
		if err := decodeClosed(m.Body, &b); err != nil {
			return err
		}
		if err := b.Validate(); err != nil {
			return err
		}
	default:
		return errors.New("unknown mailbox body")
	}
	return nil
}

func envelopeDigest(raw []byte) (string, error) {
	v, err := strictValue(raw)
	if err != nil {
		return "", err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return "", errors.New("envelope must be object")
	}
	if _, ok := obj["signature"]; !ok {
		return "", errors.New("envelope signature missing")
	}
	delete(obj, "signature")
	canonical, err := canonicalValue(obj)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(`shipmates/fleet-commander/m1/envelope-digest\0`))
	_, _ = h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c Completed) Validate() error {
	if c.Type != CompletedType || !validateID(c.DelegationID) || !validateDigest(c.ProvenanceDigest) || c.SailState == "" {
		return errors.New("invalid completed projection")
	}
	validSail := c.SailState == "not_evaluated" || c.SailState == "advisory_rejected" || c.SailState == "locally_accepted_under_existing_policy"
	if !validSail {
		return errors.New("invalid Sail state")
	}
	switch c.Result {
	case "advised":
		if c.ReasonCode != "advised" || c.AdvisoryDecision == "" || (c.AdvisoryDecision != "resume" && c.AdvisoryDecision != "continue" && c.AdvisoryDecision != "amendment_required" && c.AdvisoryDecision != "stop") || c.SailState == "not_evaluated" {
			return errors.New("invalid advised projection")
		}
	case "rejected":
		if c.ReasonCode != "response_invalid" || c.AdvisoryDecision != "" || c.SailState != "not_evaluated" {
			return errors.New("invalid rejected projection")
		}
	case "expired":
		if c.ReasonCode != "expired" || c.AdvisoryDecision != "" || c.SailState != "not_evaluated" {
			return errors.New("invalid expired projection")
		}
	case "revoked":
		if (c.ReasonCode != "revoked" && c.ReasonCode != "revoked_after_start") || c.AdvisoryDecision != "" || c.SailState != "not_evaluated" {
			return errors.New("invalid revoked projection")
		}
	case "indeterminate":
		if c.ReasonCode != "restart_after_assessment" || c.AdvisoryDecision != "" || c.SailState != "not_evaluated" {
			return errors.New("invalid indeterminate projection")
		}
	default:
		return errors.New("invalid completed result")
	}
	return nil
}

func MarshalMessage(m Message) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}
func MessageDigest(m Message) (string, error) {
	b, err := MarshalMessage(m)
	if err != nil {
		return "", err
	}
	b, err = Canonical(b)
	if err != nil {
		return "", err
	}
	return digest(b), nil
}
func digest(b []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(MailboxDigestDomain))
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func DecodeCarrier(raw []byte) (Carrier, error) {
	if len(raw) == 0 || len(raw) > MaxMessageBytes {
		return Carrier{}, errors.New("carrier exceeds bound")
	}
	if _, err := strictValue(raw); err != nil {
		return Carrier{}, err
	}
	var c Carrier
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Carrier{}, errors.New("invalid carrier")
	}
	if c.Schema != CarrierSchema || !validateID(c.FleetID) || !validateID(c.ShipID) || c.ConnectionGeneration == 0 || c.Type == "" {
		return Carrier{}, errors.New("invalid carrier fields")
	}
	switch c.Type {
	case "ship.pull.v1", "mailbox.ack.v1":
		if c.Message != nil {
			return Carrier{}, errors.New("carrier type cannot contain message")
		}
	case "fleet.delivery.v1":
		if c.Message == nil || c.Message.Validate() != nil || c.Message.Direction != FleetToShip || c.Message.FleetID != c.FleetID || c.Message.ShipID != c.ShipID {
			return Carrier{}, errors.New("invalid delivery")
		}
	case "ship.event.v1":
		if c.Message == nil || c.Message.Validate() != nil || c.Message.Direction != ShipToFleet || c.Message.FleetID != c.FleetID || c.Message.ShipID != c.ShipID || !eventProjection(c.Message.Body) {
			return Carrier{}, errors.New("invalid ship event")
		}
	default:
		return Carrier{}, errors.New("unknown carrier")
	}
	return c, nil
}

func eventProjection(raw []byte) bool {
	var body struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	return body.Type == ProgressType || body.Type == CompletedType
}

func decodeClosed(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid closed mailbox body")
	}
	return nil
}

// DecodeClosed is the shared bounded decoder for local mailbox state. It
// preserves the same duplicate-key and trailing-value rejection as the wire
// codecs before applying DisallowUnknownFields to the target type.
func DecodeClosed(raw []byte, dst any) error {
	if _, err := strictValue(raw); err != nil {
		return err
	}
	return decodeClosed(raw, dst)
}

// strictValue is the bounded M3 JSON value checker. It also supplies the
// canonical bytes used by the message digest without accepting duplicate keys.
func strictValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := readValue(dec)
	if err != nil {
		return nil, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return v, nil
}
func readValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch x := tok.(type) {
	case json.Delim:
		switch x {
		case '{':
			m := map[string]any{}
			for dec.More() {
				tok, err := dec.Token()
				k, ok := tok.(string)
				if err != nil || !ok {
					return nil, errors.New("duplicate or invalid object key")
				}
				if _, exists := m[k]; exists {
					return nil, errors.New("duplicate or invalid object key")
				}
				v, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				m[k] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return m, nil
		case '[':
			a := []any{}
			for dec.More() {
				v, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return a, nil
		default:
			return nil, errors.New("invalid delimiter")
		}
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			return nil, errors.New("non-integer number")
		}
		if _, err := strconv.ParseInt(string(x), 10, 64); err != nil {
			return nil, errors.New("invalid number")
		}
		return x, nil
	default:
		return tok, nil
	}
}

func Canonical(raw []byte) ([]byte, error) {
	v, err := strictValue(raw)
	if err != nil {
		return nil, err
	}
	return canonicalValue(v)
}
func canonicalValue(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		if x {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case string:
		return json.Marshal(x)
	case json.Number:
		return []byte(x.String()), nil
	case []any:
		var b bytes.Buffer
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			c, err := canonicalValue(e)
			if err != nil {
				return nil, err
			}
			b.Write(c)
		}
		b.WriteByte(']')
		return b.Bytes(), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b bytes.Buffer
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			c, err := canonicalValue(x[k])
			if err != nil {
				return nil, err
			}
			b.Write(c)
		}
		b.WriteByte('}')
		return b.Bytes(), nil
	default:
		return nil, fmt.Errorf("unsupported JSON value")
	}
}
