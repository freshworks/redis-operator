package utils

import (
	"fmt"

	cmclientset "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	redisfailoverclientset "github.com/freshworks/redis-operator/client/k8s/clientset/versioned"
)

// LoadKubernetesConfig loads kubernetes configuration based on flags.
func LoadKubernetesConfig(flags *CMDFlags) (*rest.Config, error) {
	var cfg *rest.Config
	// If devel mode then use configuration flag path.
	if flags.Development {
		config, err := clientcmd.BuildConfigFromFlags("", flags.KubeConfig)
		if err != nil {
			return nil, fmt.Errorf("could not load configuration: %s", err)
		}
		cfg = config
	} else {
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("error loading kubernetes configuration inside cluster, check app is running outside kubernetes cluster or run in development mode: %s", err)
		}
		cfg = config
	}

	cfg.QPS = float32(flags.K8sQueriesPerSecond)
	cfg.Burst = flags.K8sQueriesBurstable

	return cfg, nil
}

// CreateKubernetesClients creates the clients used by the operator:
// core kubernetes, the RedisFailover CRD client, the apiextensions
// client (for CRD installation/upgrade) and the cert-manager client
// (used only when spec.tls.certManager is configured on a failover).
//
// Returning nil for the cert-manager client is not supported: cert-manager
// types are always wired up. Cluster installations that do not have
// cert-manager installed simply never trigger any Certificate API calls
// because no RedisFailover opts into TLS.
func CreateKubernetesClients(flags *CMDFlags) (kubernetes.Interface, redisfailoverclientset.Interface, apiextensionsclientset.Interface, cmclientset.Interface, error) {
	config, err := LoadKubernetesConfig(flags)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	customClientset, err := redisfailoverclientset.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	aeClientset, err := apiextensionsclientset.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	cmCli, err := cmclientset.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	return clientset, customClientset, aeClientset, cmCli, nil
}
