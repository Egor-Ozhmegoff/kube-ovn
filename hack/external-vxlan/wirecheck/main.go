// wirecheck validates the Ethernet/IPv4/UDP/VXLAN captures of the isolated lab.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
)

type observation struct {
	packets [2]int
	jumbo   [2]bool
	vteps   map[string]int
}

func observe(frame []byte, observations map[uint32]*observation) error {
	if len(frame) < 14 || binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
		return nil
	}
	ip := frame[14:]
	if len(ip) < 20 || ip[0]>>4 != 4 {
		return errors.New("truncated IPv4 header")
	}
	headerLen := int(ip[0]&15) * 4
	if headerLen < 20 || len(ip) < headerLen+8 || ip[9] != 17 {
		return nil
	}
	source, destination := net.IP(ip[12:16]).String(), net.IP(ip[16:20]).String()
	fabricPrefix := os.Getenv("EXTERNAL_VXLAN_FABRIC_PREFIX")
	if fabricPrefix == "" {
		fabricPrefix = "10.110.191"
	}
	direction := 0
	if source == fabricPrefix+".100" {
		direction = 1
	} else if destination != fabricPrefix+".100" {
		return nil
	}
	localMACs := map[string]string{
		fabricPrefix + ".11": "02:ee:0b:00:00:00", fabricPrefix + ".12": "02:ee:0c:00:00:00",
		fabricPrefix + ".13": "02:ee:0d:00:00:00", fabricPrefix + ".14": "02:ee:0e:00:00:00",
	}
	if direction == 1 && localMACs[destination] == "" {
		return fmt.Errorf("unexpected local VTEP %s", destination)
	}
	if direction == 0 && localMACs[source] == "" {
		return fmt.Errorf("unexpected local VTEP %s", source)
	}
	if binary.BigEndian.Uint16(ip[6:8])&0x3fff != 0 {
		return errors.New("fragmented outer VXLAN packet")
	}
	udp := ip[headerLen:]
	port := binary.BigEndian.Uint16(udp[2:4])
	if port != 4789 {
		return fmt.Errorf("unexpected external UDP destination %d", port)
	}
	if len(udp) < 8+8+14 {
		return errors.New("truncated VXLAN packet")
	}
	vxlan := udp[8:]
	if vxlan[0]&8 == 0 {
		return errors.New("VXLAN VNI flag is unset")
	}
	vni := uint32(vxlan[4])<<16 | uint32(vxlan[5])<<8 | uint32(vxlan[6])
	if !slices.Contains([]uint32{220, 230, 504221, 504231}, vni) {
		return fmt.Errorf("unexpected external VNI %d", vni)
	}
	inner := vxlan[8:]
	if vni == 220 || vni == 230 {
		localMAC, remoteMAC := localMACs[source], "02:ff:00:00:00:01"
		if direction == 1 {
			localMAC = localMACs[destination]
		}
		wantSource, wantDestination := localMAC, remoteMAC
		if direction == 1 {
			wantSource, wantDestination = remoteMAC, localMAC
		}
		if net.HardwareAddr(inner[6:12]).String() != wantSource || net.HardwareAddr(inner[:6]).String() != wantDestination {
			return fmt.Errorf("incorrect inner Router MACs on VNI %d", vni)
		}
	}
	if observations[vni] == nil {
		observations[vni] = &observation{vteps: map[string]int{}}
	}
	seen := observations[vni]
	seen.packets[direction]++
	if direction == 0 {
		seen.vteps[source]++
	}
	if binary.BigEndian.Uint16(inner[12:14]) == 0x0800 && len(inner) >= 14+20 {
		innerIP := inner[14:]
		if binary.BigEndian.Uint16(innerIP[2:4]) == 9000 && len(innerIP) >= 9000 {
			if binary.BigEndian.Uint16(innerIP[6:8])&0x4000 != 0 {
				seen.jumbo[direction] = true
			}
		}
	}
	return nil
}

func readCapture(reader io.Reader, observations map[uint32]*observation) error {
	header := make([]byte, 24)
	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}
	var order binary.ByteOrder
	switch binary.BigEndian.Uint32(header[:4]) {
	case 0xa1b2c3d4, 0xa1b23c4d:
		order = binary.BigEndian
	case 0xd4c3b2a1, 0x4d3cb2a1:
		order = binary.LittleEndian
	default:
		return errors.New("expected classic PCAP")
	}
	if order.Uint32(header[20:24]) != 1 {
		return errors.New("expected Ethernet PCAP link type")
	}
	for {
		record := make([]byte, 16)
		if _, err := io.ReadFull(reader, record); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		length := order.Uint32(record[8:12])
		if length > 262144 || length != order.Uint32(record[12:16]) {
			return errors.New("truncated or oversized PCAP record")
		}
		frame := make([]byte, length)
		if _, err := io.ReadFull(reader, frame); err != nil {
			return err
		}
		if err := observe(frame, observations); err != nil {
			return err
		}
	}
}

func run(paths []string) error {
	if len(paths) == 0 {
		return errors.New("usage: wirecheck capture.pcap [capture.pcap ...]")
	}
	observations := map[uint32]*observation{}
	for _, path := range paths {
		file, err := os.Open(path) // #nosec G703 -- this local CLI intentionally reads caller-selected PCAP files.
		if err != nil {
			return err
		}
		parseErr := readCapture(file, observations)
		closeErr := file.Close()
		if err := errors.Join(parseErr, closeErr); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	for _, vni := range []uint32{220, 230, 504221, 504231} {
		seen := observations[vni]
		if seen == nil || seen.packets[0] == 0 || seen.packets[1] == 0 || !seen.jumbo[0] || !seen.jumbo[1] {
			return fmt.Errorf("VNI %d lacks bidirectional DF MTU9000 evidence", vni)
		}
		fmt.Printf("VNI %d: outbound=%d inbound=%d, UDP4789, DF MTU9000 in both directions\n", vni, seen.packets[0], seen.packets[1])
		if vni == 220 || vni == 230 {
			for vtep := range strings.SplitSeq(os.Getenv("EXTERNAL_VXLAN_REQUIRE_GATEWAYS"), ",") {
				if vtep != "" && seen.vteps[vtep] == 0 {
					return fmt.Errorf("VNI %d lacks egress evidence from gateway %s", vni, vtep)
				}
			}
			fmt.Printf("VNI %d egress VTEPs/RMACs verified: %v\n", vni, seen.vteps)
		}
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
