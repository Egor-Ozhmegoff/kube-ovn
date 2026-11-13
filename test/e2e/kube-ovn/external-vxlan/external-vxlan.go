package externalvxlan

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeovn/kube-ovn/test/e2e/framework"
)

// The external VTEP and two-node fixture are provisioned explicitly by
// hack/external-vxlan/kubernetes.sh on a dedicated test cluster.
var _ = framework.Describe("[group:external-vxlan]", func() {
	f := framework.NewDefaultFramework("external-vxlan")
	ginkgo.BeforeEach(func() {
		f.SkipVersionPriorTo(1, 17, "Native EVPN requires v1.17 or later")
		if os.Getenv("KUBE_OVN_EXTERNAL_VXLAN_E2E") != "true" {
			ginkgo.Skip("requires the dedicated host FRR EVPN fixture")
		}
	})
	checkTraffic := func(ctx context.Context) {
		ginkgo.GinkgoHelper()
		for _, tenant := range []int{220, 230} {
			other := 220
			if tenant == 220 {
				other = 230
			}
			for _, kind := range []string{"pods", "vm"} {
				namespace := fmt.Sprintf("evx-%s%d", kind, tenant)
				pods, err := f.ClientSet.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "external-vxlan-e2e=true"})
				framework.ExpectNoError(err)
				gomega.Expect(pods.Items).To(gomega.HaveLen(2))
				for _, pod := range pods.Items {
					command := fmt.Sprintf("lab-ping -n -c 2 -W 2 -M do -s 8972 172.20.%d.10", tenant)
					_, stderr, err := framework.ExecShellInPod(ctx, f, namespace, pod.Name, command)
					framework.ExpectNoError(err, "external VXLAN MTU9000 from %s/%s: %s", namespace, pod.Name, stderr)
					command = fmt.Sprintf("lab-ping -n -c 1 -W 1 172.20.%d.10", other)
					_, _, err = framework.ExecShellInPod(ctx, f, namespace, pod.Name, command)
					gomega.Expect(err).To(gomega.HaveOccurred(), "cross-tenant traffic must fail")
					if kind == "vm" {
						remote := "172.16.133.10"
						if tenant == 230 {
							remote = "172.16.136.200"
						}
						_, stderr, err = framework.ExecShellInPod(ctx, f, namespace, pod.Name, "lab-ping -n -c 2 -W 2 -M do -s 8972 "+remote)
						framework.ExpectNoError(err, "external L2 VXLAN MTU9000: %s", stderr)
						subnet, err := f.KubeOVNClientSet.KubeovnV1().Subnets().Get(ctx, namespace, metav1.GetOptions{})
						framework.ExpectNoError(err)
						gomega.Expect(subnet.Spec.ExternalVXLAN).NotTo(gomega.BeNil())
						output, _, err := framework.ExecShellInPod(ctx, f, namespace, pod.Name, "cat /proc/net/arp")
						framework.ExpectNoError(err)
						gomega.Expect(output).To(gomega.ContainSubstring(strings.ToLower(subnet.Spec.ExternalVXLAN.GatewayMAC)))
					}
				}
			}
		}
	}

	ginkgo.It("routes both tenants with distributed anycast gateways and MTU9000", ginkgo.Serial, checkTraffic)

	ginkgo.It("restores managed external VNI after NB drift", ginkgo.Serial, func(ctx context.Context) {
		_, _, err := framework.NBExec("ovn-nbctl", "set", "Logical_Switch", "evx-vm230", "other_config:dynamic-routing-vni=504230")
		framework.ExpectNoError(err)
		gomega.Eventually(func() string {
			output, _, err := framework.NBExec("ovn-nbctl", "get", "Logical_Switch", "evx-vm230", "other_config:dynamic-routing-vni")
			if err != nil {
				return ""
			}
			return strings.Trim(string(output), "\" \r\n")
		}, 90*time.Second, time.Second).Should(gomega.Equal("504231"))
		checkTraffic(ctx)
	})

	ginkgo.It("retains native EVPN connectivity after a CNI daemon restart", ginkgo.Serial, func(ctx context.Context) {
		pods, err := f.ClientSet.CoreV1().Pods(framework.KubeOvnNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=kube-ovn-cni"})
		framework.ExpectNoError(err)
		gomega.Expect(pods.Items).NotTo(gomega.BeEmpty())
		old := pods.Items[0]
		framework.ExpectNoError(f.ClientSet.CoreV1().Pods(old.Namespace).Delete(ctx, old.Name, metav1.DeleteOptions{}))
		gomega.Eventually(func() bool {
			current, err := f.ClientSet.CoreV1().Pods(old.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=kube-ovn-cni"})
			if err != nil || len(current.Items) != len(pods.Items) {
				return false
			}
			for _, pod := range current.Items {
				if pod.UID == old.UID || len(pod.Status.ContainerStatuses) == 0 {
					return false
				}
				for _, status := range pod.Status.ContainerStatuses {
					if !status.Ready {
						return false
					}
				}
			}
			return true
		}, 180*time.Second, time.Second).Should(gomega.BeTrue())
		checkTraffic(ctx)
	})
})
