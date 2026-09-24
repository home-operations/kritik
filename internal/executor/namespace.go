package executor

import (
	"fmt"
	"os"
	"strings"
)

// namespaceFile is where the kubelet projects the pod's namespace.
const namespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

func namespaceFromServiceAccount() (string, error) {
	b, err := os.ReadFile(namespaceFile)
	if err != nil {
		return "", fmt.Errorf("executor: pod namespace: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
