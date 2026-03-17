package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
)

const (
	outOfServiceTaintKey   = "node.kubernetes.io/out-of-service"
	outOfServiceTaintValue = "nodeshutdown"
	unreachableTaintKey    = "node.kubernetes.io/unreachable"
	notReadyTaintKey       = "node.kubernetes.io/not-ready"
)

type Config struct {
	NotReadyTimeout   time.Duration
	MaxUnhealthyPct   int
	CheckInterval     time.Duration
	Kubeconfig        string
	Namespace         string
	LeaderElectionID  string
	EnableLeaderElect bool
}

type Controller struct {
	clientset       kubernetes.Interface
	config          Config
	notReadySince   map[string]time.Time
	mu              sync.Mutex
}

func main() {
	cfg := Config{}
	flag.DurationVar(&cfg.NotReadyTimeout, "not-ready-timeout", 5*time.Minute, "Duration a node must be NotReady before adding out-of-service taint")
	flag.IntVar(&cfg.MaxUnhealthyPct, "max-unhealthy-percent", 49, "Max percentage of nodes that can be tainted before stopping (safety)")
	flag.DurationVar(&cfg.CheckInterval, "check-interval", 30*time.Second, "How often to check node states")
	flag.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "Path to kubeconfig (uses in-cluster config if empty)")
	flag.StringVar(&cfg.Namespace, "namespace", "kube-system", "Namespace for leader election lease")
	flag.StringVar(&cfg.LeaderElectionID, "leader-election-id", "kubeoos-leader", "Leader election lease name")
	flag.BoolVar(&cfg.EnableLeaderElect, "leader-elect", true, "Enable leader election for HA")
	klog.InitFlags(nil)
	flag.Parse()

	restConfig, err := buildConfig(cfg.Kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to build config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Failed to create clientset: %v", err)
	}

	ctrl := &Controller{
		clientset:     clientset,
		config:        cfg,
		notReadySince: make(map[string]time.Time),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.EnableLeaderElect {
		hostname, _ := os.Hostname()
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      cfg.LeaderElectionID,
				Namespace: cfg.Namespace,
			},
			Client: clientset.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: hostname,
			},
		}
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:            lock,
			ReleaseOnCancel: true,
			LeaseDuration:   15 * time.Second,
			RenewDeadline:   10 * time.Second,
			RetryPeriod:     2 * time.Second,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					klog.Info("Started leading")
					ctrl.run(ctx)
				},
				OnStoppedLeading: func() {
					klog.Info("Stopped leading")
					os.Exit(0)
				},
			},
		})
	} else {
		ctrl.run(ctx)
	}
}

func (c *Controller) run(ctx context.Context) {
	factory := informers.NewSharedInformerFactory(c.clientset, 0)
	nodeInformer := factory.Core().V1().Nodes().Informer()

	nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			node := newObj.(*corev1.Node)
			c.handleNodeUpdate(node)
		},
		AddFunc: func(obj interface{}) {
			node := obj.(*corev1.Node)
			c.handleNodeUpdate(node)
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	klog.Info("Node informer synced, starting reconciliation loop")

	ticker := time.NewTicker(c.config.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reconcile(ctx)
		}
	}
}

func (c *Controller) handleNodeUpdate(node *corev1.Node) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if isControlPlane(node) {
		return
	}

	if isNodeReady(node) {
		if _, tracked := c.notReadySince[node.Name]; tracked {
			klog.Infof("Node %s is Ready again, removing from tracking", node.Name)
			delete(c.notReadySince, node.Name)
		}
	} else {
		if _, tracked := c.notReadySince[node.Name]; !tracked {
			klog.Infof("Node %s is NotReady, starting tracking", node.Name)
			c.notReadySince[node.Name] = time.Now()
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) {
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("Failed to list nodes: %v", err)
		return
	}

	var workerNodes []*corev1.Node
	for i := range nodes.Items {
		if !isControlPlane(&nodes.Items[i]) {
			workerNodes = append(workerNodes, &nodes.Items[i])
		}
	}

	totalWorkers := len(workerNodes)
	if totalWorkers == 0 {
		return
	}

	// Count currently tainted nodes
	taintedCount := 0
	for _, node := range workerNodes {
		if hasOutOfServiceTaint(node) {
			taintedCount++
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for _, node := range workerNodes {
		if isNodeReady(node) {
			// Node is ready — remove out-of-service taint if present
			if hasOutOfServiceTaint(node) {
				klog.Infof("Node %s is Ready, removing out-of-service taint", node.Name)
				if err := c.removeOutOfServiceTaint(ctx, node); err != nil {
					klog.Errorf("Failed to remove taint from %s: %v", node.Name, err)
				}
			}
			delete(c.notReadySince, node.Name)
			continue
		}

		// Node is not ready
		since, tracked := c.notReadySince[node.Name]
		if !tracked {
			c.notReadySince[node.Name] = now
			continue
		}

		// Check if timeout exceeded
		if now.Sub(since) < c.config.NotReadyTimeout {
			remaining := c.config.NotReadyTimeout - now.Sub(since)
			klog.V(2).Infof("Node %s NotReady for %v, waiting %v more", node.Name, now.Sub(since).Round(time.Second), remaining.Round(time.Second))
			continue
		}

		// Already tainted
		if hasOutOfServiceTaint(node) {
			continue
		}

		// Safety check: max unhealthy threshold
		maxTainted := (totalWorkers * c.config.MaxUnhealthyPct) / 100
		if maxTainted < 1 {
			maxTainted = 1
		}
		if taintedCount >= maxTainted {
			klog.Warningf("Node %s needs out-of-service taint but %d/%d nodes already tainted (max %d%%), skipping for safety",
				node.Name, taintedCount, totalWorkers, c.config.MaxUnhealthyPct)
			continue
		}

		klog.Infof("Node %s has been NotReady for %v (threshold: %v), adding out-of-service taint",
			node.Name, now.Sub(since).Round(time.Second), c.config.NotReadyTimeout)
		if err := c.addOutOfServiceTaint(ctx, node); err != nil {
			klog.Errorf("Failed to add taint to %s: %v", node.Name, err)
		} else {
			taintedCount++
		}
	}

	// Clean up tracking for nodes that no longer exist
	nodeSet := make(map[string]bool)
	for _, node := range nodes.Items {
		nodeSet[node.Name] = true
	}
	for name := range c.notReadySince {
		if !nodeSet[name] {
			delete(c.notReadySince, name)
		}
	}
}

func (c *Controller) addOutOfServiceTaint(ctx context.Context, node *corev1.Node) error {
	fresh, err := c.clientset.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}

	if hasOutOfServiceTaint(fresh) {
		return nil
	}

	fresh.Spec.Taints = append(fresh.Spec.Taints, corev1.Taint{
		Key:    outOfServiceTaintKey,
		Value:  outOfServiceTaintValue,
		Effect: corev1.TaintEffectNoExecute,
	})

	_, err = c.clientset.CoreV1().Nodes().Update(ctx, fresh, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update node: %w", err)
	}
	klog.Infof("Added out-of-service taint to node %s", node.Name)
	return nil
}

func (c *Controller) removeOutOfServiceTaint(ctx context.Context, node *corev1.Node) error {
	fresh, err := c.clientset.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}

	if !hasOutOfServiceTaint(fresh) {
		return nil
	}

	var newTaints []corev1.Taint
	for _, t := range fresh.Spec.Taints {
		if t.Key != outOfServiceTaintKey {
			newTaints = append(newTaints, t)
		}
	}
	fresh.Spec.Taints = newTaints

	_, err = c.clientset.CoreV1().Nodes().Update(ctx, fresh, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update node: %w", err)
	}
	klog.Infof("Removed out-of-service taint from node %s", node.Name)
	return nil
}

func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func isControlPlane(node *corev1.Node) bool {
	_, ok := node.Labels["node-role.kubernetes.io/control-plane"]
	if ok {
		return true
	}
	_, ok = node.Labels["node-role.kubernetes.io/master"]
	return ok
}

func hasOutOfServiceTaint(node *corev1.Node) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == outOfServiceTaintKey {
			return true
		}
	}
	return false
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
