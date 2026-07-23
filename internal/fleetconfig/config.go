// Package fleetconfig loads the administrator-provisioned, qualifier-only Fleet
// binding. It deliberately has no network client and never exposes secrets as
// strings outside the ship-proof callback.
package fleetconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	Schema             = "shipmates.m3.qualifier-fleet-config.v1"
	CredentialSchema   = "shipmates.m3.qualifier-credential.v1"
	DefaultConfigPath  = "/etc/shipmates/m3-qualifier/fleet.json"
	DefaultTrustRoot   = "/etc/shipmates/m3-qualifier/trust"
	DefaultSecretsRoot = "/etc/shipmates/m3-qualifier/secrets"
	ServiceIdentity    = "shipmates-fleet-commander.v1"
	M7Identity         = "fleet-observation.v1"
	M3Identity         = "fleet.commander.mailbox.v1"
	maxConfigBytes     = 32 << 10
	maxTrustBytes      = 2 << 20
	maxSecretBytes     = 16 << 10
)

type Config struct {
	Schema         string `json:"schema"`
	FleetID        string `json:"fleet_id"`
	ShipID         string `json:"ship_id"`
	CredentialID   string `json:"credential_id"`
	FleetURL       string `json:"fleet_url"`
	FleetDNS       string `json:"fleet_dns"`
	TLSServerName  string `json:"tls_server_name"`
	TrustFile      string `json:"trust_file"`
	TrustSHA256    string `json:"trust_sha256"`
	SPKISHA256     string `json:"tls_spki_sha256"`
	CredentialFile string `json:"credential_file"`
	Service        string `json:"service_identity"`
	M7             string `json:"m7_identity"`
	M3             string `json:"m3_identity"`
}

// Loaded contains public bindings and private trust bytes. Credential bytes
// are retained only by the proof callback and are never fields of this value.
type Loaded struct {
	Config Config
	Trust  []byte
	secret []byte
}

type ShipBinding struct {
	Schema           string `json:"schema"`
	FleetID          string `json:"fleet_id"`
	ShipID           string `json:"ship_id"`
	CredentialID     string `json:"credential_id"`
	CredentialSecret string `json:"credential_secret,omitempty"`
	Destination      string `json:"destination"`
	Service          string `json:"service_identity"`
}

type CommanderBinding struct {
	Schema       string    `json:"schema"`
	FleetID      string    `json:"fleet_id"`
	ShipID       string    `json:"ship_id"`
	CredentialID string    `json:"credential_id"`
	SubjectID    string    `json:"subject_id"`
	Capability   string    `json:"capability"`
	Generation   uint64    `json:"generation"`
	ShipIDs      []string  `json:"ship_ids"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Revoked      bool      `json:"revoked"`
}

type commanderRecord struct {
	Schema       string    `json:"schema"`
	FleetID      string    `json:"fleet_id"`
	ShipID       string    `json:"ship_id"`
	CredentialID string    `json:"credential_id"`
	SubjectID    string    `json:"subject_id"`
	Capability   string    `json:"capability"`
	Generation   uint64    `json:"generation"`
	ShipIDs      []string  `json:"ship_ids"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Revoked      bool      `json:"revoked"`
	Secret       string    `json:"secret"`
}

func (r commanderRecord) valid(fleetID, shipID string) bool {
	now := time.Now()
	return r.Schema == "shipmates.m3.commander-credential.v1" && validID(r.CredentialID) && validID(r.SubjectID) && validSecret(r.Secret) && r.FleetID == fleetID && r.ShipID == shipID && r.Capability == "fleet.commander.delegate.v1" && r.Generation != 0 && len(r.ShipIDs) == 1 && r.ShipIDs[0] == shipID && !r.Revoked && !r.IssuedAt.IsZero() && r.IssuedAt.Before(r.ExpiresAt) && now.Before(r.ExpiresAt)
}

type Profile struct {
	Loaded
	Ship      ShipBinding
	Commander CommanderBinding
}

type SecretProof func(fleetID, shipID, credentialID string, secret []byte) error

func (l *Loaded) UseForShipProof(proof SecretProof) error {
	if proof == nil || len(l.secret) == 0 {
		return errors.New("ship proof callback unavailable")
	}
	secret := l.secret
	l.secret = nil
	defer clear(secret)
	return proof(l.Config.FleetID, l.Config.ShipID, l.Config.CredentialID, secret)
}

func Load() (Loaded, error) {
	return LoadProtectedAt(DefaultConfigPath, DefaultTrustRoot, DefaultSecretsRoot)
}

// LoadProtectedAt is the production loader. Every path component and every
// public/trust/secret file must be root-owned. It accepts an alternate
// absolute config pathname only for administrator provisioning; it never
// relaxes the ownership boundary.
func LoadProtectedAt(configPath, trustRoot, secretsRoot string) (Loaded, error) {
	return loadAt(configPath, trustRoot, secretsRoot, false)
}

// LoadAt is a deterministic fixture seam. It permits the current uid so tests
// can model protected files without root. Production code must use Load or
// LoadProtectedAt.
func LoadAt(configPath, trustRoot, secretsRoot string) (Loaded, error) {
	return loadAt(configPath, trustRoot, secretsRoot, true)
}

// LoadProfileAt validates every opened identity used by the localhost
// qualifier. Commander secret material is checked and immediately discarded;
// only its public scope is returned.
func LoadProfileAt(configPath, trustRoot, secretsRoot, shipPath, commanderPath string) (Profile, error) {
	return loadProfileAt(configPath, trustRoot, secretsRoot, shipPath, commanderPath, true)
}

// LoadProtectedProfileAt is the production profile loader and requires root
// ownership for all opened records.
func LoadProtectedProfileAt(configPath, trustRoot, secretsRoot, shipPath, commanderPath string) (Profile, error) {
	return loadProfileAt(configPath, trustRoot, secretsRoot, shipPath, commanderPath, false)
}

func loadProfileAt(configPath, trustRoot, secretsRoot, shipPath, commanderPath string, allowCurrentOwner bool) (Profile, error) {
	var loaded Loaded
	var err error
	if allowCurrentOwner {
		loaded, err = LoadAt(configPath, trustRoot, secretsRoot)
	} else {
		loaded, err = LoadProtectedAt(configPath, trustRoot, secretsRoot)
	}
	if err != nil {
		return Profile{}, err
	}
	shipBytes, err := readProtectedPath(shipPath, maxConfigBytes, 0600, allowCurrentOwner)
	if err != nil {
		return Profile{}, errors.New("ship identity unavailable")
	}
	var ship ShipBinding
	if err := decodeClosed(shipBytes, &ship); err != nil || ship.Schema != "shipmates.m3.ship-metadata.v1" || !validSecret(ship.CredentialSecret) || ship.FleetID != loaded.Config.FleetID || ship.ShipID != loaded.Config.ShipID || ship.CredentialID != loaded.Config.CredentialID || ship.Destination != loaded.Config.FleetURL || ship.Service != loaded.Config.Service {
		return Profile{}, errors.New("ship identity mismatch")
	}
	clear([]byte(ship.CredentialSecret))
	ship.CredentialSecret = ""
	commanderBytes, err := readProtectedPath(commanderPath, maxConfigBytes, 0600, allowCurrentOwner)
	if err != nil {
		return Profile{}, errors.New("commander identity unavailable")
	}
	var commander commanderRecord
	if err := decodeClosed(commanderBytes, &commander); err != nil || !commander.valid(loaded.Config.FleetID, loaded.Config.ShipID) {
		return Profile{}, errors.New("commander scope mismatch")
	}
	clear([]byte(commander.Secret))
	return Profile{Loaded: loaded, Ship: ship, Commander: CommanderBinding{Schema: commander.Schema, FleetID: commander.FleetID, ShipID: commander.ShipID, CredentialID: commander.CredentialID, SubjectID: commander.SubjectID, Capability: commander.Capability, Generation: commander.Generation, ShipIDs: append([]string(nil), commander.ShipIDs...), IssuedAt: commander.IssuedAt, ExpiresAt: commander.ExpiresAt, Revoked: commander.Revoked}}, nil
}

// LoadRuntimeProfileAt consumes public config/trust plus the two records
// delivered by systemd LoadCredential. The source credential files are never
// opened by the unprivileged process.
func LoadRuntimeProfileAt(configPath, trustRoot, shipCredentialPath, commanderCredentialPath string, allowCurrentOwner bool) (Profile, error) {
	configBytes, err := readProtectedPath(configPath, maxConfigBytes, 0644, allowCurrentOwner)
	if err != nil {
		return Profile{}, errors.New("runtime config unavailable")
	}
	var c Config
	if err := decodeClosed(configBytes, &c); err != nil || c.Validate() != nil {
		return Profile{}, errors.New("runtime config invalid")
	}
	trust, err := readProtectedChild(trustRoot, c.TrustFile, maxTrustBytes, false, allowCurrentOwner)
	if err != nil || digest(trust) != c.TrustSHA256 {
		return Profile{}, errors.New("runtime trust invalid")
	}
	shipBytes, err := readProtectedPath(shipCredentialPath, maxConfigBytes, 0600, allowCurrentOwner)
	if err != nil {
		return Profile{}, errors.New("runtime ship credential unavailable")
	}
	var ship ShipBinding
	if err := decodeClosed(shipBytes, &ship); err != nil || ship.Schema != "shipmates.m3.ship-metadata.v1" || !validSecret(ship.CredentialSecret) || ship.FleetID != c.FleetID || ship.ShipID != c.ShipID || ship.CredentialID != c.CredentialID || ship.Destination != c.FleetURL || ship.Service != c.Service {
		return Profile{}, errors.New("runtime ship binding mismatch")
	}
	commanderBytes, err := readProtectedPath(commanderCredentialPath, maxConfigBytes, 0600, allowCurrentOwner)
	if err != nil {
		return Profile{}, errors.New("runtime commander credential unavailable")
	}
	var commander commanderRecord
	if err := decodeClosed(commanderBytes, &commander); err != nil || !commander.valid(c.FleetID, c.ShipID) {
		return Profile{}, errors.New("runtime commander scope mismatch")
	}
	loaded := Loaded{Config: c, Trust: append([]byte(nil), trust...), secret: []byte(ship.CredentialSecret)}
	clear([]byte(commander.Secret))
	clear([]byte(ship.CredentialSecret))
	ship.CredentialSecret = ""
	return Profile{Loaded: loaded, Ship: ShipBinding{Schema: ship.Schema, FleetID: ship.FleetID, ShipID: ship.ShipID, CredentialID: ship.CredentialID, Destination: ship.Destination, Service: ship.Service}, Commander: CommanderBinding{Schema: commander.Schema, FleetID: commander.FleetID, ShipID: commander.ShipID, CredentialID: commander.CredentialID, SubjectID: commander.SubjectID, Capability: commander.Capability, Generation: commander.Generation, ShipIDs: append([]string(nil), commander.ShipIDs...), IssuedAt: commander.IssuedAt, ExpiresAt: commander.ExpiresAt, Revoked: commander.Revoked}}, nil
}

// LoadRuntimeProfileWithRecords is the descriptor-held credential seam. The
// caller owns the supplied buffers and must clear them after this call.
func LoadRuntimeProfileWithRecords(configPath, trustRoot string, shipBytes, commanderBytes []byte, allowCurrentOwner bool) (Profile, error) {
	configBytes, err := readProtectedPath(configPath, maxConfigBytes, 0644, allowCurrentOwner)
	if err != nil {
		return Profile{}, errors.New("runtime config unavailable")
	}
	var c Config
	if err := decodeClosed(configBytes, &c); err != nil || c.Validate() != nil {
		return Profile{}, errors.New("runtime config invalid")
	}
	trust, err := readProtectedChild(trustRoot, c.TrustFile, maxTrustBytes, false, allowCurrentOwner)
	if err != nil || digest(trust) != c.TrustSHA256 {
		return Profile{}, errors.New("runtime trust invalid")
	}
	var ship ShipBinding
	if err := decodeClosed(shipBytes, &ship); err != nil || ship.Schema != "shipmates.m3.ship-metadata.v1" || !validSecret(ship.CredentialSecret) || ship.FleetID != c.FleetID || ship.ShipID != c.ShipID || ship.CredentialID != c.CredentialID || ship.Destination != c.FleetURL || ship.Service != c.Service {
		return Profile{}, errors.New("runtime ship binding mismatch")
	}
	var commander commanderRecord
	if err := decodeClosed(commanderBytes, &commander); err != nil || !commander.valid(c.FleetID, c.ShipID) {
		return Profile{}, errors.New("runtime commander scope mismatch")
	}
	loaded := Loaded{Config: c, Trust: append([]byte(nil), trust...), secret: []byte(ship.CredentialSecret)}
	ship.CredentialSecret = ""
	return Profile{Loaded: loaded, Ship: ship, Commander: CommanderBinding{Schema: commander.Schema, FleetID: commander.FleetID, ShipID: commander.ShipID, CredentialID: commander.CredentialID, SubjectID: commander.SubjectID, Capability: commander.Capability, Generation: commander.Generation, ShipIDs: append([]string(nil), commander.ShipIDs...), IssuedAt: commander.IssuedAt, ExpiresAt: commander.ExpiresAt, Revoked: commander.Revoked}}, nil
}

func loadAt(configPath, trustRoot, secretsRoot string, allowCurrentOwner bool) (Loaded, error) {
	configBytes, err := readProtectedPath(configPath, maxConfigBytes, 0600, allowCurrentOwner)
	if err != nil {
		return Loaded{}, errors.New("fleet config unavailable")
	}
	var c Config
	if err := decodeClosed(configBytes, &c); err != nil {
		return Loaded{}, errors.New("fleet config invalid")
	}
	if err := c.Validate(); err != nil {
		return Loaded{}, err
	}
	trust, err := readProtectedChild(trustRoot, c.TrustFile, maxTrustBytes, false, allowCurrentOwner)
	if err != nil {
		return Loaded{}, errors.New("fleet trust unavailable")
	}
	if digest(trust) != c.TrustSHA256 {
		return Loaded{}, errors.New("fleet trust digest mismatch")
	}
	secretBytes, err := readProtectedChild(secretsRoot, c.CredentialFile, maxSecretBytes, true, allowCurrentOwner)
	if err != nil {
		return Loaded{}, errors.New("fleet credential unavailable")
	}
	var credential credentialRecord
	if err := decodeClosed(secretBytes, &credential); err != nil || credential.Schema != CredentialSchema || credential.FleetID != c.FleetID || credential.ShipID != c.ShipID || credential.CredentialID != c.CredentialID || !validSecret(credential.Secret) {
		return Loaded{}, errors.New("fleet credential binding mismatch")
	}
	return Loaded{Config: c, Trust: append([]byte(nil), trust...), secret: []byte(credential.Secret)}, nil
}

func (c Config) Validate() error {
	if c.Schema != Schema || !validID(c.FleetID) || !validID(c.ShipID) || !validID(c.CredentialID) || c.Service != ServiceIdentity || c.M7 != M7Identity || c.M3 != M3Identity || !validBase(c.TrustFile) || !validBase(c.CredentialFile) || !validSPKI(c.TrustSHA256) || !validSPKI(c.SPKISHA256) {
		return errors.New("invalid Fleet qualifier binding")
	}
	u, err := url.Parse(c.FleetURL)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "/api/fleet/v1/tunnel" || u.RawPath != "" || !validPort(u.Port()) || !validDNS(u.Hostname()) || u.Hostname() != c.FleetDNS || c.TLSServerName != c.FleetDNS {
		return errors.New("invalid Fleet TLS binding")
	}
	return nil
}

type credentialRecord struct {
	Schema       string `json:"schema"`
	FleetID      string `json:"fleet_id"`
	ShipID       string `json:"ship_id"`
	CredentialID string `json:"credential_id"`
	Secret       string `json:"secret"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,95}$`)
var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var secretPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{16,1000}$`)

func validID(s string) bool { return idPattern.MatchString(s) }
func validBase(s string) bool {
	return len(s) > 0 && len(s) <= 128 && !strings.ContainsAny(s, "/\\") && s != "." && s != ".." && regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(s)
}
func validSPKI(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}
func validDNS(s string) bool {
	if len(s) == 0 || len(s) > 253 || strings.HasSuffix(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !dnsLabel.MatchString(label) {
			return false
		}
	}
	return true
}
func validPort(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 1 && s[0] == '0' {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}
func validSecret(s string) bool { return len(s) <= 4096 && secretPattern.MatchString(s) }
func digest(b []byte) string    { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func clear(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func decodeClosed(data []byte, out any) error {
	if len(data) == 0 || len(data) > maxConfigBytes || !utf8.Valid(data) {
		return errors.New("invalid JSON bytes")
	}
	if err := checkNoDuplicates(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func checkNoDuplicates(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch d := tok.(type) {
		case json.Delim:
			switch d {
			case '{':
				seen := map[string]bool{}
				for dec.More() {
					key, err := dec.Token()
					if err != nil {
						return err
					}
					k, ok := key.(string)
					if !ok || seen[k] {
						return errors.New("duplicate JSON key")
					}
					seen[k] = true
					if err := walk(); err != nil {
						return err
					}
				}
				_, err = dec.Token()
				return err
			case '[':
				for dec.More() {
					if err := walk(); err != nil {
						return err
					}
				}
				_, err = dec.Token()
				return err
			}
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func readProtectedPath(path string, limit int, perm uint32, allowCurrentOwner bool) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("unsafe path")
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	return readProtectedChild(dir, base, limit, perm == 0600, allowCurrentOwner)
}

func readProtectedChild(root, base string, limit int, secret, allowCurrentOwner bool) ([]byte, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !validBase(base) {
		return nil, errors.New("unsafe protected path")
	}
	dfd, err := openDirNoFollow(root, allowCurrentOwner)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dfd)
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(dfd, base, flags, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), base)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("protected file descriptor unavailable")
	}
	defer file.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 || !trustedOwner(st.Uid, allowCurrentOwner) || st.Mode&0022 != 0 || (secret && st.Mode&0077 != 0) {
		return nil, errors.New("unsafe protected file")
	}
	b, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(b) > limit {
		return nil, errors.New("protected file too large")
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || after.Dev != st.Dev || after.Ino != st.Ino || after.Nlink != st.Nlink {
		return nil, errors.New("protected file replaced")
	}
	var pathAfter unix.Stat_t
	if err := unix.Fstatat(dfd, base, &pathAfter, unix.AT_SYMLINK_NOFOLLOW); err != nil || pathAfter.Dev != st.Dev || pathAfter.Ino != st.Ino || pathAfter.Nlink != st.Nlink {
		return nil, errors.New("protected path replaced")
	}
	return b, nil
}

func openDirNoFollow(path string, allowCurrentOwner bool) (int, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, errors.New("unsafe protected directory")
	}
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			_ = unix.Close(fd)
			return -1, errors.New("unsafe protected directory")
		}
		next, openErr := unix.Openat(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, openErr
		}
		var st unix.Stat_t
		// Fixture paths often begin below a user-owned temporary directory (and
		// may traverse a user-namespace-mapped /).  The production path still
		// requires every component to be root-owned; fixture mode retains the
		// no-follow/type/write checks without asserting host ownership.
		if statErr := unix.Fstat(next, &st); statErr != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || (!allowCurrentOwner && !trustedOwner(st.Uid, false)) || st.Mode&0022 != 0 {
			_ = unix.Close(next)
			_ = unix.Close(fd)
			return -1, errors.New("unsafe protected directory")
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, nil
}

func trustedOwner(uid uint32, allowCurrentOwner bool) bool {
	return uid == 0 || (allowCurrentOwner && int(uid) == os.Getuid())
}
