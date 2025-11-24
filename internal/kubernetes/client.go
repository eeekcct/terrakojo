package kubernetes

import (
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/eeekcct/terrakojo/api/v1alpha1"
)

// NewClient creates a new Kubernetes client with proper scheme configuration
func NewClient() (client.Client, error) {
	config, err := config.GetConfig()
	if err != nil {
		return nil, err
	}

	// Setup scheme with both standard Kubernetes resources and custom resources
	scheme := runtime.NewScheme()
	// Add standard Kubernetes resources
	_ = clientgoscheme.AddToScheme(scheme)
	// Add custom resources (CRDs)
	_ = v1alpha1.AddToScheme(scheme)

	k8sClient, err := client.New(config, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		return nil, err
	}

	return k8sClient, nil
}
