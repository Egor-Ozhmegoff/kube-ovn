package northbound

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/barrier"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/host"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
)

// This component test combines the production admission barrier, a real NB DB
// and kernel ingress filtering. It does not emulate Cisco or a KubeVirt VM.
func TestDelayedFilterAdmissionPackets(t *testing.T) {
	if os.Getenv("KUBE_OVN_DISPOSABLE_LAB") != "true" {
		t.Skip("requires privileged disposable network-none container")
	}
	_, err := os.Stat("/.dockerenv")
	require.NoError(t, err)
	require.Zero(t, os.Geteuid())
	db := testDatabase(t)
	subnet, store, nodes := admissionFixture(t, db)
	ctx := t.Context()
	ip := func(args ...string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	ip("link", "add", "oplf10101", "type", "veth", "peer", "name", "fabric")
	ip("link", "add", "exit", "type", "veth", "peer", "name", "endpoint")
	ip("link", "add", "component-br", "type", "bridge")
	for _, name := range []string{"oplf10101", "exit"} {
		ip("link", "set", name, "master", "component-br")
	}
	for _, name := range []string{"oplf10101", "fabric", "exit", "endpoint", "component-br"} {
		ip("link", "set", name, "up")
	}
	first, second := "02:00:00:00:01:11", "02:00:00:00:01:12"
	require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, &openperouter.MACGuard{}, first))
	require.NoError(t, AdmitMACs(ctx, db, subnet, store))
	cm, state, err := store.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, host.EnsureReflection(ctx, string(subnet.UID), "oplf10101", []string{first}))
	for _, node := range nodes {
		require.NoError(t, store.JoinClosed(ctx, cm.UID, node.UID, node.Name, "session"))
		require.NoError(t, store.InstallIntent(ctx, cm.UID, node.UID, "session", state.Revision))
		require.NoError(t, store.Acknowledge(ctx, cm.UID, node.UID, "session", state.Revision))
	}
	require.NoError(t, AdmitMACs(ctx, db, subnet, store, first))
	require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, &openperouter.MACGuard{}, second))
	require.ErrorIs(t, AdmitMACs(ctx, db, subnet, store, second), barrier.ErrPending)
	_, state, err = store.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, store.Acknowledge(ctx, cm.UID, nodes[0].UID, "session", state.Revision))
	fabric, endpoint := packetSocket(t, "fabric"), packetSocket(t, "endpoint")
	// Delay B's installation deliberately: the old filter really accepts this
	// MAC, demonstrating that polling/atomic replacement alone cannot protect it.
	packet := testEthernet(t, second, 0x86dd)
	require.True(t, packetDelivered(t, fabric, endpoint, "fabric", packet))
	port := &ovnnb.LogicalSwitchPort{UUID: "new_endpoint", Name: "new-endpoint", Addresses: []string{second + " 10.101.1.12"}}
	activate := func() error {
		if err := AdmitMACs(ctx, db, subnet, store, second); err != nil {
			return err
		}
		ls := &ovnnb.LogicalSwitch{Name: subnet.Name}
		if err := readRow(ctx, db, ls); err != nil {
			return err
		}
		ops, err := db.Table(port).CreateOps(port)
		if err != nil {
			return err
		}
		attach, err := db.Table(ls).MutateOps(ls, model.Mutation{Field: &ls.Ports, Mutator: ovsdb.MutateOperationInsert, Value: []string{port.UUID}})
		if err != nil {
			return err
		}
		return db.Table(ls).Transact(ctx, "activate-endpoint", append(ops, attach...)...)
	}
	require.ErrorIs(t, activate(), barrier.ErrPending)
	require.Error(t, readRow(ctx, db, port), "new endpoint cannot transmit before B's filter is installed")
	require.NoError(t, host.EnsureReflection(ctx, string(subnet.UID), "oplf10101", []string{first, second}))
	require.NoError(t, store.Acknowledge(ctx, cm.UID, nodes[1].UID, "session", state.Revision))
	require.NoError(t, activate())
	require.Eventually(t, func() bool { return readRow(ctx, db, port) == nil }, time.Second, 10*time.Millisecond)
	for _, kind := range []uint16{0x88b5, 0x0806, 0x86dd, 0x8035} {
		t.Run(fmt.Sprintf("%04x", kind), func(t *testing.T) {
			require.False(t, packetDelivered(t, fabric, endpoint, "fabric", testEthernet(t, second, kind)), "reflected Ethernet/ARP/NA/RARP must drop")
			require.True(t, packetDelivered(t, fabric, endpoint, "fabric", testEthernet(t, "02:aa:00:00:00:01", kind)), "legitimate external L2 must pass")
		})
	}
	require.True(t, packetDelivered(t, endpoint, fabric, "endpoint", testEthernet(t, second, 0x8035)), "outbound migration RARP must pass")
}

func packetSocket(t *testing.T, name string) int {
	t.Helper()
	link, err := netlink.LinkByName(name)
	require.NoError(t, err)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK, int(uint16(0x0300)))
	require.NoError(t, err)
	require.NoError(t, unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: 0x0300, Ifindex: link.Attrs().Index}))
	t.Cleanup(func() { require.NoError(t, unix.Close(fd)) })
	return fd
}

func testEthernet(t *testing.T, source string, kind uint16) []byte {
	t.Helper()
	mac, err := net.ParseMAC(source)
	require.NoError(t, err)
	frame := make([]byte, 96)
	copy(frame, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(frame[6:], mac)
	binary.BigEndian.PutUint16(frame[12:14], kind)
	if kind == 0x0806 || kind == 0x8035 {
		binary.BigEndian.PutUint16(frame[14:16], 1)
		binary.BigEndian.PutUint16(frame[16:18], 0x0800)
		frame[18], frame[19] = 6, 4
		opcode := uint16(1)
		if kind == 0x8035 {
			opcode = 3
		}
		binary.BigEndian.PutUint16(frame[20:22], opcode)
		copy(frame[22:28], mac)
		copy(frame[28:32], net.ParseIP("10.101.1.12").To4())
		copy(frame[32:38], mac)
		copy(frame[38:42], net.ParseIP("10.101.1.11").To4())
	}
	if kind == 0x86dd { // IPv6 unsolicited NA, valid checksum.
		copy(frame[:6], []byte{0x33, 0x33, 0, 0, 0, 1})
		frame[14], frame[20], frame[21], frame[54] = 0x60, 58, 255, 136
		binary.BigEndian.PutUint16(frame[18:20], 32)
		copy(frame[22:38], net.ParseIP("fd00:101:1::12").To16())
		copy(frame[38:54], net.ParseIP("ff02::1").To16())
		frame[58] = 0x20
		copy(frame[62:78], frame[22:38])
		frame[78], frame[79] = 2, 1
		copy(frame[80:86], mac)
		sum := uint32(32 + 58)
		for i := 22; i < 86; i += 2 {
			sum += uint32(binary.BigEndian.Uint16(frame[i : i+2]))
		}
		for sum>>16 != 0 {
			sum = (sum & 65535) + (sum >> 16)
		}
		binary.BigEndian.PutUint16(frame[56:58], ^uint16(sum))
	}
	return frame
}

func packetDelivered(t *testing.T, sender, receiver int, name string, packet []byte) bool {
	t.Helper()
	buffer := make([]byte, 2048)
	for {
		_, _, err := unix.Recvfrom(receiver, buffer, unix.MSG_DONTWAIT)
		if err != nil {
			break
		}
	}
	link, err := netlink.LinkByName(name)
	require.NoError(t, err)
	require.NoError(t, unix.Sendto(sender, packet, 0, &unix.SockaddrLinklayer{Ifindex: link.Attrs().Index}))
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		_, err := unix.Poll([]unix.PollFd{{Fd: int32(receiver), Events: unix.POLLIN}}, 20)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		require.NoError(t, err)
		n, _, err := unix.Recvfrom(receiver, buffer, unix.MSG_DONTWAIT)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		require.NoError(t, err)
		if n >= len(packet) && bytes.Equal(buffer[:len(packet)], packet) {
			return true
		}
	}
	return false
}
