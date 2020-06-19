package main

import (
	"context"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sort"
	"strconv"
	"sync"
	"time"

	//v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func deletePods(c client.Client, log logr.Logger, node corev1.Node) error {
	prioritizedPods := map[int][]corev1.Pod{}

	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node.Name),
	}); err != nil {
		return err
	}

	if len(pods.Items) == 0 {
		log.Info("No pods scheduled on this node")

		return nil
	}

	for _, pod := range pods.Items {
		if pod.Namespace == "kube-system" {
			log.V(1).Info("Skipping daemonset in this namespace", "pod", pod.Name, "namespace", pod.Namespace)

			continue
		}

		var pri int

		priStr, ok := pod.Annotations[PodAnnotationKeyPodDeletionPriority]
		if ok {
			var err error

			pri, err = strconv.Atoi(priStr)
			if err != nil {
				return err
			}
		} else {
			pri = 0
		}

		if _, ok := prioritizedPods[pri]; !ok {
			prioritizedPods[pri] = []corev1.Pod{}
		}

		prioritizedPods[pri] = append(prioritizedPods[pri], pod)
	}

	decreasingPriorities := []int{}

	for pri := range prioritizedPods {
		decreasingPriorities = append(decreasingPriorities, pri)
	}

	sort.Slice(decreasingPriorities, func(i, j int) bool {
		return i > j
	})

	for _, pri := range decreasingPriorities {
		pods := prioritizedPods[pri]

		var wg sync.WaitGroup

		for i := range pods {
			po := pods[i]

			mylog := log.WithValues("priority", pri, "pod_namespace", po.Namespace, "pod_name", po.Name)

			if po.DeletionTimestamp == nil {
				mylog.Info("deletionTimestamp not set. Deleting pod")

				if err := c.Delete(context.Background(), &po); err != nil {
					return err
				}
			} else {
				mylog.Info("deletionTimestamp already set. Skipped deleting pod")
			}

			wg.Add(1)
			go func() {
				defer wg.Done()

				var latestPo corev1.Pod

				for {
					mylog.Info("Waiting for pod to disappear")
					if err := c.Get(context.Background(), types.NamespacedName{Namespace: po.Namespace, Name: po.Name}, &latestPo); apierrors.IsNotFound(err) {
						mylog.Info("Waiting for pod to disappear... Done")
						break
					}

					time.Sleep(3 * time.Second)
				}
			}()
		}

		wg.Wait()
	}

	return nil
}

func DeletePods(c client.Client, log logr.Logger, node corev1.Node) error {
	return deletePods(c, log, node)
}
