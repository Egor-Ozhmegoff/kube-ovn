//go:build openperouterlab

// This fixture is only for the disposable physical OVN component lab.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	nb "github.com/kubeovn/kube-ovn/pkg/openperouter/northbound"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	spec := &v1.OpenPERouterIntegration{Mode: openperouter.Mode, VRF: "tenant-a", L3VNIName: "tenant-a", L3VNI: 10001, TransitCIDRs: []string{"169.254.32.0/20", "fd00:32::/64"}, HostSessionCIDRs: []string{"169.254.16.0/20", "fd00:16::/64"}, RouterASN: 64514, HostASN: 64515}
	vpc := &v1.Vpc{ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", UID: "disposable-vpc-a"}, Spec: v1.VpcSpec{OpenPERouter: spec}}
	transit, err := openperouter.BuildTransit(vpc.Name, spec)
	if err != nil {
		return err
	}
	a, err := openperouter.BuildNode(vpc.Name, "node-a", 11, spec)
	if err != nil {
		return err
	}
	b, err := openperouter.BuildNode(vpc.Name, "node-b", 12, spec)
	if err != nil {
		return err
	}
	if len(os.Args) == 2 && os.Args[1] == "describe" {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"transit": transit, "node-a": a, "node-b": b, "l2Port": openperouter.L2Port("blue")})
	}
	if len(os.Args) != 3 || os.Args[1] != "nb" {
		return fmt.Errorf("usage: helper describe | helper nb endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cm, err := ovnnb.FullDatabaseModel()
	if err != nil {
		return err
	}
	logger := logr.Discard()
	raw, err := client.NewOVSDBClient(cm, client.WithEndpoint(os.Args[2]), client.WithLeaderOnly(false), client.WithLogger(&logger))
	if err != nil {
		return err
	}
	if err = raw.Connect(ctx); err != nil {
		return err
	}
	defer raw.Close()
	if _, err = raw.MonitorAll(ctx); err != nil {
		return err
	}
	db := table.NewDatabase(table.Wrap(raw), 5*time.Second, table.RetryPolicy{})
	lr := &ovnnb.LogicalRouter{UUID: "fixture_router", Name: vpc.Name}
	var ops []ovsdb.Operation
	for _, network := range []struct{ name, cidr, gw, mac string }{
		{"pods", "10.101.3.0/24", "10.101.3.1/24", "02:00:05:00:03:00"},
		{"blue", "10.101.1.0/24", "10.101.1.1/24", "02:00:05:00:03:00"},
	} {
		lrp := &ovnnb.LogicalRouterPort{UUID: network.name + "_lrp", Name: "tenant-a-" + network.name, MAC: network.mac, Networks: []string{network.gw, fmt.Sprintf("fd00:101:%d::1/64", map[string]int{"pods": 3, "blue": 1}[network.name])}}
		routerPort := &ovnnb.LogicalSwitchPort{UUID: network.name + "_router_port", Name: network.name + "-tenant-a", Type: "router", Addresses: []string{"router"}, Options: map[string]string{"router-port": lrp.Name}}
		ls := &ovnnb.LogicalSwitch{Name: network.name, Ports: []string{routerPort.UUID}}
		rows := []model.Model{lrp, routerPort}
		for i, node := range []string{"node-a", "node-b"} {
			suffix := []string{"a", "b"}[i]
			prefix := "10.101.3"
			if network.name == "blue" {
				prefix = "10.101.1"
			}
			mac := fmt.Sprintf("02:00:00:00:%02x:%02x", map[string]int{"pods": 3, "blue": 1}[network.name], 17+i)
			port := &ovnnb.LogicalSwitchPort{UUID: network.name + "_" + suffix, Name: network.name + "-" + suffix, Addresses: []string{fmt.Sprintf("%s %s.%d fd00:101:%d::%d", mac, prefix, 11+i, map[string]int{"pods": 3, "blue": 1}[network.name], 11+i)}, Options: map[string]string{"requested-chassis": node}}
			ls.Ports = append(ls.Ports, port.UUID)
			rows = append(rows, port)
		}
		rows = append(rows, ls)
		lr.Ports = append(lr.Ports, lrp.UUID)
		for _, row := range rows {
			o, e := db.Table(row).CreateOps(row)
			if e != nil {
				return e
			}
			ops = append(ops, o...)
		}
	}
	o, err := db.Table(lr).CreateOps(lr)
	if err != nil {
		return err
	}
	ops = append(ops, o...)
	if err = db.Table(lr).Transact(ctx, "disposable-fixture", ops...); err != nil {
		return err
	}
	var current *ovnnb.LogicalRouter
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		current, err = table.GetByName(ctx, db, lr, lr.Name, "router", true, func(r *ovnnb.LogicalRouter) string { return r.Name })
		if err != nil {
			return err
		}
		if current != nil {
			break
		}
	}
	if current == nil {
		return fmt.Errorf("fixture router not observed")
	}
	var nodes []*corev1.Node
	for i, name := range []string{"node-a", "node-b"} {
		nodes = append(nodes, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{openperouter.NodeIndexAnnotation: fmt.Sprint(11 + i), openperouter.NodeRouterMACAnnotation: fmt.Sprintf("02:00:00:01:00:%02x", 11+i)}}})
	}
	defaults, err := nb.EnsureTransit(ctx, db, vpc, nodes, "10.101.3.0/24", "10.101.1.0/24", "fd00:101:3::/64", "fd00:101:1::/64")
	if err != nil {
		return err
	}
	subnet := &v1.Subnet{ObjectMeta: metav1.ObjectMeta{Name: "blue", UID: "disposable-blue"}, Spec: v1.SubnetSpec{Vpc: vpc.Name, CIDRBlock: "10.101.1.0/24", Gateway: "10.101.1.1", OpenPERouterL2: &v1.OpenPERouterL2Attachment{L2VNIName: "tenant-a-blue", VNI: 10101, GatewayMAC: "02:00:05:00:03:00"}}}
	if err = nb.EnsureL2(ctx, db, vpc, subnet, &openperouter.MACGuard{}); err != nil {
		return err
	}
	var routeOps []ovsdb.Operation
	for i, d := range defaults {
		route := &ovnnb.LogicalRouterStaticRoute{UUID: fmt.Sprintf("fixture_default_%d", i), IPPrefix: d.Prefix, Nexthop: d.NextHop}
		o, e := db.Table(route).CreateOps(route)
		if e != nil {
			return e
		}
		routeOps = append(routeOps, o...)
		o, e = db.Table(current).MutateOps(current, model.Mutation{Field: &current.StaticRoutes, Mutator: ovsdb.MutateOperationInsert, Value: []string{route.UUID}})
		if e != nil {
			return e
		}
		routeOps = append(routeOps, o...)
	}
	return db.Table(current).Transact(ctx, "disposable-default", routeOps...)
}
