package host

import (
	"os"

	"golang.org/x/sys/unix"
)

// Lock serializes ownership across daemon restarts on the same OVS instance.
// No new process can publish Closed while an old process could reopen an edge.
func Lock() (*os.File, error) {
	file, err := os.OpenFile("/var/run/openvswitch/kube-ovn-openperouter.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, closeErr
		}
		return nil, err
	}
	return file, nil
}
