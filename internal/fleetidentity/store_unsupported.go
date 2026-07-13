//go:build !unix

package fleetidentity

func readAuthority(string) ([]byte, bool, error) { return nil, false, storageError() }
func writeAuthority(string, []byte) error        { return storageError() }

func planShipState(string) (StatePlan, error)   { return StatePlan{}, storageError() }
func readShipState(string) ([]byte, error)      { return nil, storageError() }
func writeShipState(string, []byte, bool) error { return storageError() }
