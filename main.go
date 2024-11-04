package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"reflect"
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
	defaultTargets := getDefaultTargets(cfg.Targets)

	// read current nodes to targets
	tries := 0
	watcherNodesReopenTries := 0
	watcherConfigMapReopenTries := 0
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
		klog.Fatalf("Failed to watch Nodes: %v", err)
	}
	defer watcher.Stop()

	// watch coonfigMap
	watcherConfigMap, err := clientset.CoreV1().ConfigMaps(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", configMapName),
	})
	if err != nil {
		klog.Fatalf("Failed to watch ConfigMap '%s': %v", configMapName, err)
	}
	defer watcherConfigMap.Stop()

	timer := time.NewTimer(5 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		
		// Get nodes by timeout
		case <-timer.C:
			klog.Infof("Get nodes by timeout")
			currentTargets, err := getNodesIP(&ctx, clientset)
			if err != nil {
				klog.Errorf("Failed to get nodes IP: %v", err)
				continue
			}
			newTargets := uniqueTargets(append(defaultTargets, currentTargets...))
			
			if compareTargets(cfg.Targets, newTargets) {
				err = updateConfigMap(&ctx, clientset, namespace, configMapName, cfg)
				if err != nil {
					klog.Errorf("Failed to update ConfigMap: %v", err)
				}
			}

			timer.Reset(5 * time.Minute)

		// Watch nodes
		case event := <-watcher.ResultChan():
			if event.Object == nil {
				klog.Error("The watch channel nodes has been closed")
				watcher.Stop()
				watcher, err = clientset.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{})
				if err != nil {
					klog.Errorf("Failed to reopen watch channel: %v", err)
					watcherNodesReopenTries++
					time.Sleep(1 * time.Second)
					if watcherNodesReopenTries > 20 {
						klog.Fatalf("Failed to reopen watch channel for nodes after 20 tries")
					}
					continue
				}
				watcherNodesReopenTries = 0
				continue
			}

			node, ok := event.Object.(*corev1.Node)
			if !ok {
				klog.Errorf("Unexpected object type: %T", event.Object)
				time.Sleep(1 * time.Second)
				continue
			}

			switch event.Type {

			case watch.Added:
				if !nodeExistsInTargets(*node, cfg.Targets) {
					klog.Infof("New node added: %s", node.Name)
					cfg.Targets = uniqueTargets(addNodeToTargets(*node, cfg.Targets))
					err = updateConfigMap(&ctx, clientset, namespace, configMapName, cfg)
					if err != nil {
						klog.Errorf("Failed to update ConfigMap: %v", err)
					}
				}

			case watch.Deleted:
				cfg.Targets = uniqueTargets(removeNodeFromTargets(*node, cfg.Targets))
				err = updateConfigMap(&ctx, clientset, namespace, configMapName, cfg)
				if err != nil {
					klog.Errorf("Failed to update ConfigMap: %v", err)
				}
				klog.Infof("Node deleted: %s", node.Name)
			}
		
		// Watch ConfigMap
		case event := <-watcherConfigMap.ResultChan():

			if event.Object == nil {
				klog.Error("The watch channel configMap has been closed")
				watcherConfigMap.Stop()
				watcherConfigMap, err = clientset.CoreV1().ConfigMaps(namespace).Watch(ctx, metav1.ListOptions{
					FieldSelector: fmt.Sprintf("metadata.name=%s", configMapName),
				})
				if err != nil {
					klog.Errorf("Failed to reopen watch channel: %v", err)
					watcherConfigMapReopenTries++
					time.Sleep(1 * time.Second)
					if watcherConfigMapReopenTries > 20 {
						klog.Fatalf("Failed to reopen watch channel for configMap after 20 tries")
					}
					continue
				}
				watcherConfigMapReopenTries = 0
				continue
			}

			configMap, ok := event.Object.(*corev1.ConfigMap)
			if !ok {
				klog.Errorf("Unexpected object type: %T", event.Object)
				time.Sleep(1 * time.Second)
				continue
			}
			
			switch event.Type {
			case watch.Modified:
				klog.Infof("ConfigMap modified")
				lastFieldManager := getLastFieldManager(configMap)
				klog.Infof("Last field manager: %s", lastFieldManager)

				if lastFieldManager != fieldManager {
					klog.Infof("ConfigMap was changed by another the field manager: %s. Reread the config", lastFieldManager)
					cfg := getConfig(&ctx, clientset, namespace, configMapName)
					defaultTargets = getDefaultTargets(cfg.Targets)
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
				}
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
	tries := 0
	for {
		var err error
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
	_, err := clientset.CoreV1().ConfigMaps(namespace).Get(*ctx, configMapName, metav1.GetOptions{})

	create := false
	if err != nil && !k8sErrors.IsNotFound(err) {
		klog.Infof("[updateConfigMap]: failed to get ConfigMap: %v. Create new configMap", err)
		create = true
	} 
	

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

	if create {
		_, err = clientset.CoreV1().ConfigMaps(namespace).Create(*ctx, configMap, metav1.CreateOptions{
			FieldManager: fieldManager,
		})
	} else {
		_, err = clientset.CoreV1().ConfigMaps(configMap.Namespace).Update(*ctx, configMap, metav1.UpdateOptions{
			FieldManager: fieldManager,
		})
	}

	if err != nil {
		return fmt.Errorf("[updateConfigMap]: Failed to apply or create ConfigMap: %v", err)
	} else {
		klog.Info("[updateConfigMap]: ConfigMap apply/create successfully")
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
	externalIP := ""
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			internalIP = address.Address
		}
		if address.Type == corev1.NodeExternalIP {
			externalIP = address.Address
		}
	}
	nodeIP := internalIP
	if nodeIP == "" {
		nodeIP = externalIP
	}

	if nodeIP != "" {
		targets = append(targets, pconfig.TargetConfig{
			Addr: nodeIP,
			Labels: map[string]string{
				"target_name": node.Name,
				"added_by":    fieldManager,
			},
		})
	}
	return targets
}

// Check if a node exists in the targets
func nodeExistsInTargets(node corev1.Node, targets []pconfig.TargetConfig) bool {
	nodeName := node.Name
	for _, target := range targets {
		if target.Labels["target_name"] == nodeName {
			return true
		}
	}
	return false
}

func compareTargets(targets1 []pconfig.TargetConfig, targets2 []pconfig.TargetConfig) bool {
	return reflect.DeepEqual(targets1, targets2)
}

func getLastFieldManager(configMap *corev1.ConfigMap) string {
	lastFieldManager := ""
	lastModified := time.Time{}
	for _, field := range configMap.ManagedFields {
		if field.Time.After(lastModified) {
			lastModified = field.Time.Time
			lastFieldManager = field.Manager
		}
	}
	return lastFieldManager
}

func getDefaultTargets(targets []pconfig.TargetConfig) []pconfig.TargetConfig {
	defaultTargets := make([]pconfig.TargetConfig, 0)
	for _, target := range targets {
		if _, exists := target.Labels["added_by"]; !exists || target.Labels["added_by"] != fieldManager {
			defaultTargets = append(defaultTargets, target)
		}
	}
	return defaultTargets
}