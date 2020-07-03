package main

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"math/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetupTest will set up a testing environment.
// This includes:
// * creating a Namespace to be used during the test
// * starting the 'RunnerReconciler'
// * stopping the 'RunnerSetReconciler" after the test ends
// Call this function at the start of each of your tests.
func SetupTest(ctx context.Context) *corev1.Namespace {
	var stopCh chan struct{}
	ns := &corev1.Namespace{}

	BeforeEach(func() {
		stopCh = make(chan struct{})
		*ns = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "testns-" + randStringRunes(5)},
		}

		err := k8sClient.Create(ctx, ns)
		Expect(err).NotTo(HaveOccurred(), "failed to create test namespace")

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{})
		Expect(err).NotTo(HaveOccurred(), "failed to create manager")

		controller := &NodeController{
			Client:   mgr.GetClient(),
			Scheme:   k8sscheme.Scheme,
			Log:      logf.Log,
			recorder: mgr.GetEventRecorderFor("node-detacher"),
		}
		err = controller.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "failed to setup controller")

		go func() {
			defer GinkgoRecover()

			err := mgr.Start(stopCh)
			Expect(err).NotTo(HaveOccurred(), "failed to start manager")
		}()
	})

	AfterEach(func() {
		close(stopCh)

		err := k8sClient.Delete(ctx, ns)

		Expect(err).NotTo(HaveOccurred(), "failed to delete test namespace")

		var nodes corev1.NodeList

		{
			err := k8sClient.List(context.Background(), &nodes)

			Expect(err).NotTo(HaveOccurred(), "failed to list test nodes")
		}

		for _, no := range nodes.Items {
			err := k8sClient.Delete(context.Background(), &no)

			Expect(err).NotTo(HaveOccurred(), "failed to delete test node")
		}
	})

	return ns
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz1234567890")

func randStringRunes(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

var _ = Context("Inside of a new namespace", func() {
	ctx := context.TODO()
	ns := SetupTest(ctx)

	Describe("when no existing resources exist", func() {

		It("should do nothing on schedulable nodes", func() {
			name := "schedulable-node"

			{
				rs := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: corev1.NodeSpec{
						Unschedulable: false,
					},
				}

				err := k8sClient.Create(ctx, rs)

				Expect(err).NotTo(HaveOccurred(), "failed to create test node resource")

				node := corev1.Node{}

				Eventually(
					func() bool {
						err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &node)
						if err != nil {
							logf.Log.Error(err, "list nodes")
						}

						return node.Spec.Unschedulable
					},
					time.Second*5, time.Millisecond*500).Should(BeEquivalentTo(false))
			}
		})

		It("should not fail on unschedulable nodes", func() {
			name := "unschedulable-node"

			{
				rs := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: name,
					},
					Spec: corev1.NodeSpec{
						Unschedulable: true,
					},
				}

				err := k8sClient.Create(ctx, rs)

				Expect(err).NotTo(HaveOccurred(), "failed to create test node resource")

				node := corev1.Node{}

				Eventually(
					func() bool {
						err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &node)
						if err != nil {
							logf.Log.Error(err, "list nodes")
						}

						return node.Spec.Unschedulable
					},
					time.Second*5, time.Millisecond*500).Should(BeEquivalentTo(true))
			}
		})

		It("should delete non kube-system pods on unschedulable node", func() {
			name := "unschedulable-node"

			{
				objs := []runtime.Object{
					&corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "pod-to-delete",
							Namespace: ns.Name,
							Annotations: map[string]string{
								PodAnnotationDisableEviction: "true",
							},
						},
						Spec: corev1.PodSpec{
							NodeName: name,
							Containers: []corev1.Container{
								{
									Name:  "primary",
									Image: "nginx:latest",
								},
							},
						},
					},
					&corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: name,
						},
						Spec: corev1.NodeSpec{},
					},
				}

				for _, obj := range objs {
					err := k8sClient.Create(ctx, obj)

					Expect(err).NotTo(HaveOccurred(), "failed to create test node resource")
				}

				node := corev1.Node{}

				{
					err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &node)

					Expect(err).NotTo(HaveOccurred(), "failed to get test node")

					node.Spec.Unschedulable = true

					updateErr := k8sClient.Update(ctx, &node)

					Expect(updateErr).NotTo(HaveOccurred(), "failed to update test node")
				}

				Eventually(
					func() *metav1.Time {
						var po corev1.Pod

						err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: "pod-to-delete"}, &po)
						if err != nil {
							logf.Log.Error(err, "getting pod")
						}

						return po.DeletionTimestamp
					},
					time.Second*5, time.Millisecond*500).Should(Not(BeNil()), "Pod DeletionTimestamp")

				{
					var po corev1.Pod

					err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: "pod-to-delete"}, &po)

					Expect(err).NotTo(HaveOccurred(), "failed to get pod")

					Expect(po.DeletionTimestamp).NotTo(BeNil())

					var zero int64 = 0
					deleteErr := k8sClient.Delete(ctx, &po, &client.DeleteOptions{
						GracePeriodSeconds: &zero,
					})

					Expect(deleteErr).NotTo(HaveOccurred(), "failed to delete po")
				}

				Eventually(
					func() bool {
						err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &node)
						if err != nil {
							logf.Log.Error(err, "list nodes")
						}

						taints := node.Spec.Taints

						var found bool

						for _, t := range taints {
							if t.Key == NodeTaintKeyDetaching {
								found = true
								break
							}
						}

						return found
					},
					time.Second*5, time.Millisecond*500).Should(BeEquivalentTo(true))

				Eventually(
					func() bool {
						err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &node)
						if err != nil {
							logf.Log.Error(err, "list nodes")
						}

						a := node.ObjectMeta.Annotations[NodeAnnotationKeyDetaching]

						return a == "true"
					},
					time.Second*5, time.Millisecond*500).Should(BeEquivalentTo(true))

				Eventually(
					func() bool {
						err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &node)
						if err != nil {
							logf.Log.Error(err, "list nodes")
						}

						ts := node.ObjectMeta.Annotations[NodeAnnotationKeyDetachmentTimestamp]

						var found *time.Time

						if ts != "" {
							t, err := time.Parse(time.RFC3339, ts)
							if err != nil {
								logf.Log.Error(err, fmt.Sprintf("parsing timestamp %q", ts))
							} else {
								found = &t
							}
						}

						return found != nil
					},
					time.Second*5, time.Millisecond*500).Should(BeEquivalentTo(true))
			}
		})

		It("should delete non kube-system pods on node being deleted by cluster-autoscaler", func() {
			name := "unschedulable-node"

			{
				objs := []runtime.Object{
					&corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "pod-to-delete",
							Namespace: ns.Name,
							Annotations: map[string]string{
								PodAnnotationDisableEviction: "true",
							},
						},
						Spec: corev1.PodSpec{
							NodeName: name,
							Containers: []corev1.Container{
								{
									Name:  "primary",
									Image: "nginx:latest",
								},
							},
						},
					},
					&corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: name,
						},
						Spec: corev1.NodeSpec{},
					},
				}

				for _, obj := range objs {
					err := k8sClient.Create(ctx, obj)

					Expect(err).NotTo(HaveOccurred(), "failed to create test node resource")
				}

				node := corev1.Node{}

				{
					err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &node)

					Expect(err).NotTo(HaveOccurred(), "failed to get test node")

					node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
						Key:       NodeTaintToBeDeletedByCA,
						Value:     "",
						Effect:    "NoSchedule",
						TimeAdded: nil,
					})

					updateErr := k8sClient.Update(ctx, &node)

					Expect(updateErr).NotTo(HaveOccurred(), "failed to update test node")
				}

				Eventually(
					func() *metav1.Time {
						var po corev1.Pod

						err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: "pod-to-delete"}, &po)
						if err != nil {
							logf.Log.Error(err, "getting pod")
						}

						return po.DeletionTimestamp
					},
					time.Second*5, time.Millisecond*500).Should(Not(BeNil()), "Pod DeletionTimestamp")

				{
					var po corev1.Pod

					err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: "pod-to-delete"}, &po)

					Expect(err).NotTo(HaveOccurred(), "failed to get pod")

					Expect(po.DeletionTimestamp).NotTo(BeNil())

					var zero int64 = 0
					deleteErr := k8sClient.Delete(ctx, &po, &client.DeleteOptions{
						GracePeriodSeconds: &zero,
					})

					Expect(deleteErr).NotTo(HaveOccurred(), "failed to delete po")
				}

				Eventually(
					func() bool {
						err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &node)
						if err != nil {
							logf.Log.Error(err, "list nodes")
						}

						taints := node.Spec.Taints

						var found bool

						for _, t := range taints {
							if t.Key == NodeTaintKeyDetaching {
								found = true
								break
							}
						}

						return found
					},
					time.Second*5, time.Millisecond*500).Should(BeEquivalentTo(true))
			}
		})

	})
})
