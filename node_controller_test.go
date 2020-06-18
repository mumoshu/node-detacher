package main

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"math/rand"
	ctrl "sigs.k8s.io/controller-runtime"
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
	_ = SetupTest(ctx)

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

	})
})
