package kubernetes

import (
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Client wraps the Kubernetes client and controller-runtime client
type Client struct {
	KubeClient    *kubernetes.Clientset
	RuntimeClient client.Client
}

// NewClient creates a new Kubernetes client
func NewClient(kubeClient *kubernetes.Clientset, runtimeClient client.Client) *Client {
	return &Client{
		KubeClient:    kubeClient,
		RuntimeClient: runtimeClient,
	}
}
