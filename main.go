package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	pconfig "github.com/czerwonk/ping_exporter/config"
	"gopkg.in/yaml.v2"
)

const (
	fieldManager         = "dynamic-config-ping-exporter"
	defaultConfigMapName = "ping-exporter-config"
)

func main() {

	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to get in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create client: %v", err)
	}

	defaultNamespace, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		klog.Fatalf("Failed to read namespace: %v", err)
	}

	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = string(defaultNamespace)
	}

	configMapName := os.Getenv("CONFIGMAP")
	if configMapName == "" {
		configMapName = defaultConfigMapName
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle termination signals to gracefully stop the program
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stopCh
		cancel()
	}()

	cfg := getConfig(&ctx, clientset, namespace, configMapName)
	defaultTargets := cfg.Targets

	// read current nodes to targets
	tries := 0
	for {
		currentTargets, err := getNodesIP(&ctx, clientset)
		if err != nil {
			klog.Errorf("Failed to get nodes IP: %v", err)
			time.Sleep(5 * time.Second)
			tries++

			if tries > 9 {
				klog.Fatalf("Failed to get nodes IP after 10 tries")
			}
			continue
		}
		cfg.Targets = uniqueTargets(append(defaultTargets, currentTargets...))
		break
	}

	tries = 0
	for {
		err = updateConfigMap(&ctx, clientset, namespace, configMapName, cfg)
		if err != nil {
			klog.Errorf("Failed to update ConfigMap: %v. Sleep 5 seconds and try again", err)
			time.Sleep(5 * time.Second)
			tries++

			if tries > 9 {
				klog.Fatalf("Failed to update ConfigMap after 10 tries")
			}
			continue
		}
		break
	}

	// watch nodes
	watcher, err := clientset.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("Failed to watch Nodes: %v", err)
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-time.After(5 * time.Minute):
			currentTargets, err := getNodesIP(&ctx, clientset)
			if err != nil {
				klog.Errorf("Failed to get nodes IP: %v", err)
				continue
			}
			cfg.Targets = uniqueTargets(append(defaultTargets, currentTargets...))
			err = updateConfigMap(&ctx, clientset, namespace, configMapName, cfg)
			if err != nil {
				klog.Errorf("Failed to update ConfigMap: %v", err)
			}

		case event := <-watcher.ResultChan():

			node, ok := event.Object.(*corev1.Node)
			if !ok {
				klog.Errorf("Unexpected object type: %T", event.Object)
				continue
			}

			switch event.Type {

			case watch.Added:
				klog.Infof("Node added: %s", node.Name)
				cfg.Targets = uniqueTargets(addNodeToTargets(*node, cfg.Targets))
				err = updateConfigMap(&ctx, clientset, namespace, configMapName, cfg)
				if err != nil {
					klog.Errorf("Failed to update ConfigMap: %v", err)
				}

			case watch.Modified:
				klog.Infof("Node modified: %s", node.Name)
			case watch.Deleted:
				cfg.Targets = uniqueTargets(removeNodeFromTargets(*node, cfg.Targets))
				err = updateConfigMap(&ctx, clientset, namespace, configMapName, cfg)
				if err != nil {
					klog.Errorf("Failed to update ConfigMap: %v", err)
				}
				klog.Infof("Node deleted: %s", node.Name)
			}
		}
	}
}

func getNodesIP(ctx *context.Context, clientset *kubernetes.Clientset) ([]pconfig.TargetConfig, error) {

	nodes, err := clientset.CoreV1().Nodes().List(*ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("[getNodesIP]: failed to list nodes: %v", err)
	}

	targets := make([]pconfig.TargetConfig, 0)
	for _, node := range nodes.Items {
		targets = addNodeToTargets(node, targets)
	}
	return targets, nil
}

func getConfig(ctx *context.Context, clientset *kubernetes.Clientset, namespace string, configMapName string) pconfig.Config {

	// try to get the ConfigMap
	var configMap *corev1.ConfigMap
	found := false
	for {
		var err error
		tries := 0

		configMap, err = clientset.CoreV1().ConfigMaps(namespace).Get(*ctx, configMapName, metav1.GetOptions{})

		if err != nil && !k8sErrors.IsNotFound(err) {
			klog.Errorf("[getConfig]: failed to get ConfigMap: %v. Slep 5 seconds and try again", err)
			time.Sleep(5 * time.Second)

			tries++
			if tries > 9 {
				break
			}
			continue
		} else if err == nil {
			found = true
		}

		break
	}

	defaultConfigYaml := `
targets: []
dns:
	refresh: 3m
ping:
	history-size: 15
	interval: 5s
	payload-size: 64
	timeout: 1s
`

	useDefaultConfig := func() pconfig.Config {
		var cfg pconfig.Config
		err := yaml.Unmarshal([]byte(defaultConfigYaml), &cfg)
		if err != nil {
			klog.Fatalf("[getConfig]: failed to parse default YAML config: %s, err: %v. Please write to developer", defaultConfigYaml, err)
		}
		return cfg
	}

	var cfg pconfig.Config

	if found {
		configYAML, ok := configMap.Data["config.yaml"]
		if ok {
			err := yaml.Unmarshal([]byte(configYAML), &cfg)
			if err != nil {
				klog.Errorf("[getConfig]: failed to parse YAML from configMap '%s' key '%s': %v. Use default config: %s", configMapName, namespace, err, defaultConfigYaml)
				return useDefaultConfig()
			}
			return cfg
		} else {
			klog.Errorf("[getConfig]: key 'config.yaml' not found in ConfigMap '%s', use default config: %s", configMapName, defaultConfigYaml)
			return useDefaultConfig()
		}
	} else {
		klog.Errorf("[getConfig]: ConfigMap '%s' not found, use default config: %s", configMapName, defaultConfigYaml)
		return useDefaultConfig()
	}
}

func updateConfigMap(ctx *context.Context, clientset *kubernetes.Clientset, namespace string, configMapName string, cfg pconfig.Config) error {

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("[updateConfigMap]: Failed to marshal config to YAML: %v", err)
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: namespace,
		},
		Data: map[string]string{
			"config.yaml": string(data),
		},
	}
	_, err = clientset.CoreV1().ConfigMaps(configMap.Namespace).Update(*ctx, configMap, metav1.UpdateOptions{
		FieldManager: fieldManager,
	})

	if err != nil {
		return fmt.Errorf("[updateConfigMap]: Failed to apply ConfigMap: %v", err)
	} else {
		klog.Info("[updateConfigMap]: ConfigMap apply successfully")
	}

	return nil
}

func uniqueTargets(targets []pconfig.TargetConfig) []pconfig.TargetConfig {
	encountered := map[string]bool{}
	result := []pconfig.TargetConfig{}
	for v := range targets {
		if !encountered[targets[v].Addr] {
			encountered[targets[v].Addr] = true
			result = append(result, targets[v])
		}
	}
	return result
}

// Remove node from targets
func removeNodeFromTargets(node corev1.Node, targets []pconfig.TargetConfig) []pconfig.TargetConfig {
	nodeName := node.Name
	var updatedTargets []pconfig.TargetConfig
	for _, target := range targets {
		if target.Labels["target_name"] != nodeName {
			updatedTargets = append(updatedTargets, target)
		}
	}
	return updatedTargets
}

func addNodeToTargets(node corev1.Node, targets []pconfig.TargetConfig) []pconfig.TargetConfig {
	internalIP := ""
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			internalIP = address.Address
			break
		}
	}
	if internalIP != "" {
		targets = append(targets, pconfig.TargetConfig{
			Addr: internalIP,
			Labels: map[string]string{
				"target_name": node.Name,
			},
		})
	}
	return targets
}
