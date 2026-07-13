package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errRoutingDestinationChanged = errors.New("routing destination changed at commit point")

func validateRoutingFilesystem(root string, j *routingJournal) error {
	for _, rel := range []string{Dir, filepath.Join(Dir, routingTransactionName), filepath.Join(Dir, routingTransactionName, "journal.json")} {
		if err := routingSafeNode(filepath.Join(root, rel), rel != filepath.Join(Dir, routingTransactionName, "journal.json")); err != nil {
			return err
		}
	}
	for _, e := range j.Entries {
		for _, rel := range []string{e.Path, filepath.Join(Dir, routingTransactionName, e.Stage), filepath.Join(Dir, routingTransactionName, e.Backup)} {
			if err := routingSafeOptionalFile(filepath.Join(root, rel)); err != nil {
				return err
			}
		}
	}
	return nil
}

func routingSafeNode(p string, wantDir bool) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || (wantDir && !info.IsDir()) || (!wantDir && !info.Mode().IsRegular()) || (!wantDir && routingLinkCount(info) != 1) {
		return fmt.Errorf("unsafe routing transaction entry")
	}
	return nil
}

func routingSafeOptionalFile(p string) error {
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || routingLinkCount(info) != 1 {
		return errors.New("unsafe routing transaction file")
	}
	return nil
}
