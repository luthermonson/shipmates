package recovery

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
)

// CommanderDelegationConfig is the local, disabled-by-default policy for the
// M1 ship-local delegation slice. It is deliberately part of recovery config
// so project configuration does not create a package import cycle with the
// implementation package.
type CommanderDelegationConfig struct {
	Enabled          bool                    `yaml:"enabled" json:"enabled"`
	FleetID          string                  `yaml:"fleetId" json:"fleet_id"`
	ProtocolVersion  uint8                   `yaml:"protocolVersion" json:"protocol_version"`
	MaxOfferSeconds  uint32                  `yaml:"maxOfferSeconds" json:"max_offer_seconds"`
	PreExecHelper    string                  `yaml:"preExecHelper,omitempty" json:"pre_exec_helper,omitempty"`
	PermittedIssuers []CommanderIssuerConfig `yaml:"permittedIssuers" json:"permitted_issuers"`
}

type CommanderIssuerConfig struct {
	KeyID     string `yaml:"keyId" json:"key_id"`
	PublicKey string `yaml:"publicKey" json:"public_key"`
	Revoked   bool   `yaml:"revoked" json:"revoked"`
}

var commanderKeyID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{15,95}$`)
var commanderFleetID = commanderKeyID

func (c CommanderDelegationConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !commanderFleetID.MatchString(c.FleetID) || c.ProtocolVersion != 1 || c.MaxOfferSeconds == 0 || c.MaxOfferSeconds > 600 || len(c.PermittedIssuers) == 0 || len(c.PermittedIssuers) > 32 {
		return errors.New("invalid commander delegation policy")
	}
	if len(c.PreExecHelper) > 512 || strings.ContainsAny(c.PreExecHelper, "\r\n") {
		return errors.New("invalid commander pre-exec helper")
	}
	seen := map[string]bool{}
	for _, issuer := range c.PermittedIssuers {
		if !commanderKeyID.MatchString(issuer.KeyID) || seen[issuer.KeyID] {
			return errors.New("invalid commander issuer policy")
		}
		seen[issuer.KeyID] = true
		key, err := base64.RawURLEncoding.DecodeString(issuer.PublicKey)
		if err != nil || len(key) != 32 {
			return errors.New("invalid commander issuer public key")
		}
	}
	return nil
}
