package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubectl/pkg/drain"
)

// getControllerOfPod finds the controller owner reference for a given pod.
// Pods managed by Deployments, ReplicaSets, StatefulSets, etc., will have a controller.
// We ignore pods that are not managed by a controller.
func getControllerOfPod(pod *v1.Pod) *metav1.OwnerReference {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return &ref
		}
	}
	return nil
}

// getPodsToDrain identifies all pods on the target node that has all containers in ready status
// and are managed by a controller. These are the pods we expect to be rescheduled.
func getPodsToDrain(ctx context.Context, clientset *kubernetes.Clientset, nodeName string) (map[types.UID]*v1.Pod, error) {
	log.Printf("Step 1: Finding all running, controller-managed pods on node '%s'...", nodeName)

	// List all pods in the cluster
	podList, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	podsToDrain := make(map[types.UID]*v1.Pod)
	for i := range podList.Items {
		pod := &podList.Items[i]
		// We only care about pods that are currently running
		if allContainersReady(pod) {
			// Check if the pod is managed by a controller (e.g., ReplicaSet)
			controllerRef := getControllerOfPod(pod)
			if controllerRef != nil {
				if controllerRef.Kind != "DaemonSet" {
					log.Printf("  - Found running pod '%s/%s' managed by %s '%s'", pod.Namespace, pod.Name, controllerRef.Kind, controllerRef.Name)
					podsToDrain[pod.UID] = pod
				} else {
					log.Printf("  - Skipping pod '%s/%s' as it is managed by a DaemonSet.", pod.Namespace, pod.Name)
				}
			} else {
				log.Printf("  - Skipping pod '%s/%s' as it is not managed by a controller.", pod.Namespace, pod.Name)
			}
		}
	}

	if len(podsToDrain) == 0 {
		log.Println("No running, controller-managed pods found on the node. Nothing to do.")
	}
	return podsToDrain, nil
}

// cordonNode marks the specified node as unschedulable.
func cordonNode(ctx context.Context, clientset *kubernetes.Clientset, nodeName string) error {
	log.Printf("Step 2: Cordoning node '%s' to prevent new pods from being scheduled on it.", nodeName)
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	// If already cordoned, do nothing
	if node.Spec.Unschedulable {
		log.Printf("Node '%s' is already cordoned.", nodeName)
		return nil
	}

	// Create a patch to update the node's schedulable status
	patch := []byte(`{"spec":{"unschedulable":true}}`)
	_, err = clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to cordon node %s: %w", nodeName, err)
	}
	log.Printf("Successfully cordoned node '%s'.", nodeName)
	return nil
}

// evictPod creates an eviction request for a given pod.
func evictPod(ctx context.Context, clientset *kubernetes.Clientset, pod *v1.Pod) error {
	log.Printf("  - Attempting to evict pod '%s/%s'...", pod.Namespace, pod.Name)
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	// The subresource for eviction is in the policy/v1 API group
	err := clientset.PolicyV1().Evictions(pod.Namespace).Evict(ctx, eviction)
	if err != nil {
		return fmt.Errorf("failed to evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	log.Printf("  - Eviction request for pod '%s/%s' successful.", pod.Namespace, pod.Name)
	return nil
}

// waitForReplacementPod watches for a new pod managed by the same controller to become "Running".
func waitForReplacementPod(ctx context.Context, clientset *kubernetes.Clientset, evictedPod *v1.Pod, controllerRef *metav1.OwnerReference) error {
	log.Printf("  - Watching for a replacement for pod '%s/%s'...", evictedPod.Namespace, evictedPod.Name)

	// Set up a watcher for pod events in the same namespace as the evicted pod
	watcher, err := clientset.CoreV1().Pods(evictedPod.Namespace).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to create watcher for replacement pod: %w", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		// We only care about Added or Modified events
		if event.Type != watch.Added && event.Type != watch.Modified {
			continue
		}

		newPod, ok := event.Object.(*v1.Pod)
		if !ok {
			continue
		}

		// A replacement pod must meet several criteria:
		// 1. It must not be the same pod we just evicted (UID will be different).
		// 2. It must be managed by the same controller.
		// 3. All of its containers must be ready.
		// 4. It must be scheduled on a DIFFERENT node.
		newPodControllerRef := getControllerOfPod(newPod)
		if newPod.UID != evictedPod.UID &&
			time.Since(newPod.ObjectMeta.CreationTimestamp.Time).Minutes() < 10 &&
			newPodControllerRef != nil && newPodControllerRef.UID == controllerRef.UID &&
			allContainersReady(newPod) &&
			newPod.Spec.NodeName != evictedPod.Spec.NodeName {
			log.Printf("  - Success! Found replacement pod '%s/%s' running on node '%s'.", newPod.Namespace, newPod.Name, newPod.Spec.NodeName)
			return nil
		}
	}
	return fmt.Errorf("watcher channel closed before replacement pod was found")
}

// allContainersReady checks if all containers in a pod have a "Ready" status of true.
func allContainersReady(pod *v1.Pod) bool {
	// If there are no container statuses yet, it's definitely not ready.
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return false // Found a container that is not ready
		}
	}
	return true // All containers are ready
}

func main() {
	var (
		kubeconfig string
		nodeName   string
		timeout    time.Duration
	)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&nodeName, "node", "", "The name of the node to drain and verify.")
	flag.DurationVar(&timeout, "timeout", 10*time.Minute, "Timeout for waiting for all pods to be replaced.")
	flag.Parse()

	if nodeName == "" {
		log.Fatal("You must specify the node name using the -node flag.")
	}

	// Build configuration from the kubeconfig file
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		log.Fatalf("Error building kubeconfig: %v", err)
	}

	// Create a new Kubernetes clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating Kubernetes clientset: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 1. Get the list of pods that need to be drained from the target node.
	podsToDrain, err := getPodsToDrain(ctx, clientset, nodeName)
	if err != nil {
		log.Fatalf("Error getting pods to drain: %v", err)
	}

	// 2. Cordon the node to prevent new pods from being scheduled there.
	if len(podsToDrain) != 0 {
		if err := cordonNode(ctx, clientset, nodeName); err != nil {
			log.Fatalf("Error cordoning node: %v", err)
		}
	}

	// 3. Evict each pod and wait for its replacement.
	if len(podsToDrain) != 0 {
		log.Printf("Step 3: Evicting %d pods and verifying replacements...", len(podsToDrain))
		for _, pod := range podsToDrain {
			for {
				if err := evictPod(ctx, clientset, pod); err != nil {
					log.Printf("WARNING: Failed to evict pod %s/%s: %v. Waiting for 10s before retry...", pod.Namespace, pod.Name, err)
					time.Sleep(10 * time.Second)
				} else {
					break
				}
			}

			// Wait for the replacement to be scheduled and running
			controllerRef := getControllerOfPod(pod) // We know this is not nil from getPodsToDrain
			if err := waitForReplacementPod(ctx, clientset, pod, controllerRef); err != nil {
				log.Fatalf("FATAL: Failed to find replacement for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			}
		}
	}

	// 4. Perform the final drain using the kubectl drain library
	log.Printf("Step 4: Performing final drain on node '%s' with specified options...", nodeName)
	drainHelper := &drain.Helper{
		Ctx:                 ctx,
		Client:              clientset,
		Force:               true,
		GracePeriodSeconds:  -1, // Use the pod's terminationGracePeriodSeconds
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  true,
		Timeout:             timeout,
		Out:                 io.Writer(os.Stdout),
		ErrOut:              io.Writer(os.Stderr),
	}

	if err := drain.RunNodeDrain(drainHelper, nodeName); err != nil {
		log.Fatalf("FATAL: Failed to complete node drain: %v", err)
	}
}
