package openperouter

import (
	"crypto/sha256"
	"fmt"
)

// ReflectionAddressSet is shared by the NB producer and the node's scoped SB
// monitor. Only identities in this subnet are needed by its ingress filter.
func ReflectionAddressSet(uid, subnet string) string {
	hash := sha256.Sum256([]byte(uid + "/" + subnet))
	return fmt.Sprintf("oprm_%x", hash[:12])
}
