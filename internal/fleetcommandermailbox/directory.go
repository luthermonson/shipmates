//go:build unix

package fleetcommandermailbox

import (
	"errors"
	"sync"

	"github.com/luthermonson/shipmates/internal/fleetcommander"
)

var ErrInvalidIdentity = errors.New("invalid commander mailbox identity")

// Directory lazily owns one durable mailbox per authenticated ship. The
// authenticated tunnel supplies the ship key; callers cannot select another
// partition through a message body.
type Directory struct {
	root, fleetID string
	mu            sync.Mutex
	stores        map[string]*Store
}

func OpenDirectory(root, fleetID string) (*Directory, error) {
	if !validID(fleetID) {
		return nil, ErrInvalidIdentity
	}
	return &Directory{root: root, fleetID: fleetID, stores: make(map[string]*Store)}, nil
}

func (d *Directory) store(shipID string) (*Store, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.stores[shipID]; s != nil {
		return s, nil
	}
	s, err := Open(d.root, d.fleetID, shipID, Limits{})
	if err != nil {
		return nil, err
	}
	d.stores[shipID] = s
	return s, nil
}

func (d *Directory) PullCommander(shipID string, ack uint64) (*fleetcommander.Message, error) {
	s, err := d.store(shipID)
	if err != nil {
		return nil, err
	}
	return s.PullCommander(ack)
}

func (d *Directory) AckCommander(shipID string, ack uint64) error {
	s, err := d.store(shipID)
	if err != nil {
		return err
	}
	return s.AckCommander(ack)
}

func (d *Directory) IngestCommanderEvent(shipID string, event fleetcommander.Message) error {
	s, err := d.store(shipID)
	if err != nil {
		return err
	}
	return s.IngestCommanderEvent(event)
}
