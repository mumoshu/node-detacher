package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type KubernetesService interface {
	getUnschedulableNodes() ([]corev1.Node, error)
}

type kubernetesSvc struct {
	clientset        *kubernetes.Clientset
	ignoreDaemonSets bool
	deleteLocalData  bool
}

func kubeGetClientset() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		if err == rest.ErrNotInCluster {
			config, err = getKubeOutOfCluster()
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("Error getting kubernetes config from within cluster")
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

func getKubeOutOfCluster() (*rest.Config, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		if home := homeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		} else {
			return nil, fmt.Errorf("Not KUBECONFIG provided and no home available")
		}
	}

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err.Error())
	}
	return config, nil
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func createK8sService() (KubernetesService, error) {
	clientset, err := kubeGetClientset()
	if err != nil {
		log.Fatalf("Error getting kubernetes connection: %v", err)
	}
	if clientset == nil {
		return nil, nil
	}
	return &kubernetesSvc{clientset: clientset}, nil
}

func (svc *kubernetesSvc) getUnschedulableNodes() ([]corev1.Node, error) {
	if svc.clientset == nil {
		return nil, nil
	}

	nodeList, err := svc.clientset.CoreV1().Nodes().List(v1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("Unexpected error getting kubernetes nodes: %v", err)
	}

	var unschedulables []corev1.Node

	for _, node := range nodeList.Items {
		if isUnschedulable(node) {
			unschedulables = append(unschedulables, node)
		}
	}

	return unschedulables, nil
}

func getInstanceID(node corev1.Node) (string, error) {
	labels := node.GetLabels()

	instanceID, ok := labels["alpha.eksctl.io/instance-id"]
	if !ok {
		return "", fmt.Errorf("node must be labeled with `alpha.eksctl.io/instance-id` for this operator to work")
	}

	return instanceID, nil
}

func isUnschedulable(node corev1.Node) bool {
	return node.Spec.Unschedulable
}
