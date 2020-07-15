package main

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sort"
	"strconv"
	"sync"
	"time"

	//v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func deletePods(c client.Client, c2 v1.CoreV1Interface, log logr.Logger, node corev1.Node) error {
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
		var pri int

		priStr, ok := pod.Annotations[PodAnnotationKeyPodDeletionPriority]
		if ok {
			var err error

			pri, err = strconv.Atoi(priStr)
			if err != nil {
				return err
			}
		} else {
			log.V(1).Info(fmt.Sprintf("Skipping pod without %q annotation", PodAnnotationKeyPodDeletionPriority), "pod", types.NamespacedName{
				Namespace: pod.Namespace,
				Name:      pod.Name,
			})

			continue
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

				var evict bool

				if po.Annotations == nil || po.Annotations[PodAnnotationDisableEviction] != "true" {
					evict = true
				}

				if evict {
					mylog.Info("evicting pod due to that the disable-eviction annotation is set to true")

					eviction := &v1beta1.Eviction{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: po.Namespace,
							Name:      po.Name,
						},
					}

					GracePeriod := 30 * time.Second

					gracePeriod := &GracePeriod

					if gracePeriod != nil {
						gracePeriodSeconds := int64(gracePeriod.Seconds())
						eviction.DeleteOptions = &metav1.DeleteOptions{
							GracePeriodSeconds: &gracePeriodSeconds,
						}
					}

					if err := c2.Pods(po.Namespace).Evict(eviction); err != nil {
						mylog.Error(err, "evicting pod")

						if !apierrors.IsNotFound(err) {
							return err
						}
					}
				} else {
					mylog.Info("deleting pod without taking PDB into account due to that the disable-eviction annotation is not set")

					if err := c.Delete(context.Background(), &po); err != nil {
						return err
					}
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

func DeletePods(c client.Client, c2 v1.CoreV1Interface, log logr.Logger, node corev1.Node) error {
	return deletePods(c, c2, log, node)
}
