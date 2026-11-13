package util

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

const ExternalVXLANValidatedAnnotation = "ovn.kubernetes.io/external-vxlan-validated"

type externalVXLANHostAttachment struct {
	UID          types.UID
	VNI          uint32
	FRR          *kubeovnv1.ExternalVXLANFRR
	RoutingPorts []string
}

// ExternalVXLANHostContract fingerprints the complete host interface contract.
// It excludes status, egress eligibility and transit IPs, which do not change
// host-owned devices. It is an acknowledgement of validation, not a health lease.
func ExternalVXLANHostContract(node *corev1.Node, subnets []kubeovnv1.Subnet, vpcs []kubeovnv1.Vpc) (string, error) {
	if node.Annotations[ExternalVXLANIPAnnotation] == "" {
		return "", fmt.Errorf("node %s has no external VTEP identity", node.Name)
	}
	attachments := map[string]externalVXLANHostAttachment{}
	for _, subnet := range subnets {
		if config := subnet.Spec.ExternalVXLAN; config != nil && config.FRR != nil && subnet.DeletionTimestamp.IsZero() {
			attachments["subnet/"+subnet.Name] = externalVXLANHostAttachment{UID: subnet.UID, VNI: config.VNI, FRR: config.FRR}
		}
	}
	for _, vpc := range vpcs {
		if config := vpc.Spec.ExternalVXLAN; config != nil && config.FRR != nil && vpc.DeletionTimestamp.IsZero() {
			attachment := externalVXLANHostAttachment{UID: vpc.UID, VNI: config.VNI, FRR: config.FRR}
			for _, gateway := range ExternalVXLANGatewayNodes(config) {
				if gateway.Node == node.Name {
					attachment.RoutingPorts = append(attachment.RoutingPorts, gateway.RoutingPort)
				}
			}
			attachments["vpc/"+vpc.Name] = attachment
		}
	}
	if len(attachments) == 0 {
		return "", nil
	}
	data, err := json.Marshal(struct {
		Version     int
		NodeUID     types.UID
		BootID      string
		IP, MAC     string
		Attachments map[string]externalVXLANHostAttachment
	}{1, node.UID, node.Status.NodeInfo.BootID, node.Annotations[ExternalVXLANIPAnnotation], node.Annotations[ExternalVXLANMACAnnotation], attachments})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func ValidateExternalVXLANHostContract(node *corev1.Node, subnets []kubeovnv1.Subnet, vpcs []kubeovnv1.Vpc) error {
	want, err := ExternalVXLANHostContract(node, subnets, vpcs)
	if err != nil {
		return err
	}
	if want == "" || node.Annotations[ExternalVXLANValidatedAnnotation] != want {
		return fmt.Errorf("node %s has not validated the current external VXLAN host contract", node.Name)
	}
	return nil
}
