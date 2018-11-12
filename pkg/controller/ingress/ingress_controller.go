package ingress

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	ingressv1alpha1 "github.com/openshift/cluster-ingress-operator/pkg/apis/ingress/v1alpha1"
	"github.com/openshift/cluster-ingress-operator/pkg/manifests"
	"github.com/openshift/cluster-ingress-operator/pkg/util"
	"github.com/openshift/cluster-ingress-operator/pkg/util/slice"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// installerConfigNamespace is the namespace containing the installer config.
	installerConfigNamespace = "kube-system"

	// clusterConfigResource is the resource containing the installer config.
	clusterConfigResource = "cluster-config-v1"

	// ClusterIngressFinalizer is applied to all ClusterIngress resources before
	// they are considered for processing; this ensures the operator has a chance
	// to handle all states.
	// TODO: Make this generic and not tied to the "default" ingress.
	ClusterIngressFinalizer = "ingress.openshift.io/default-cluster-ingress"
)

// Add creates a new ClusterIngress Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	r := newReconciler(mgr)

	// Create a new controller
	c, err := controller.New("openshift-ingress-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource ClusterIngress
	err = c.Watch(&source.Kind{Type: &ingressv1alpha1.ClusterIngress{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &appsv1.DaemonSet{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

// newReconciler returns a new reconcile.Reconciler and esnures default cluster ingress
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	client, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		logrus.Fatalf("failed to get k8s client: %v", err)
	}
	ic, err := util.GetInstallConfig(client)
	if err != nil {
		logrus.Fatalf("failed to get installconfig: %v", err)
	}
	namespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		logrus.Fatalf("failed to get watch namespace: %v", err)
	}

	rc := &IngressReconciler{
		client:          mgr.GetClient(),
		scheme:          mgr.GetScheme(),
		installConfig:   ic,
		manifestFactory: manifests.NewFactory(),
		namespace:       namespace,
	}
	if err := rc.ensureDefaultClusterIngress(); err != nil {
		logrus.Fatalf("failed to ensure default cluster ingress: %v", err)
	}

	return rc
}

var _ reconcile.Reconciler = &IngressReconciler{}

// IngressReconciler reconciles a ClusterIngress object
type IngressReconciler struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme

	installConfig   *util.InstallConfig
	manifestFactory *manifests.Factory
	namespace       string
}

// ensureDefaultClusterIngress ensures that a default ClusterIngress exists.
func (r *IngressReconciler) ensureDefaultClusterIngress() error {
	ci, err := r.manifestFactory.DefaultClusterIngress(r.installConfig)
	if err != nil {
		return err
	}

	err = r.client.Create(context.TODO(), ci)
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	} else if err == nil {
		logrus.Infof("created default clusteringress %s/%s", ci.Namespace, ci.Name)
	}
	return nil
}

// Reconcile reads that state of the cluster for a ClusterIngress object and makes changes based on the state read
// and what is in the ClusterIngress.Spec
func (r *IngressReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	defer r.syncOperatorStatus()
	logrus.Infof("reconciling ClusterIngress %s/%s\n", request.Namespace, request.Name)

	// Fetch the ClusterIngress instance
	ci := &ingressv1alpha1.ClusterIngress{}
	err := r.client.Get(context.TODO(), request.NamespacedName, ci)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Ensure we have all the necessary scaffolding on which to place router instances.
	if err := r.ensureRouterNamespace(); err != nil {
		return reconcile.Result{}, err
	}

	if ci.DeletionTimestamp != nil {
		// Destroy any router associated with the clusteringress.
		err := r.ensureRouterDeleted(ci)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to delete clusteringress %s/%s: %v", ci.Namespace, ci.Name, err)
		}
		// Clean up the finalizer to allow the clusteringress to be deleted.
		if slice.ContainsString(ci.Finalizers, ClusterIngressFinalizer) {
			ci.Finalizers = slice.RemoveString(ci.Finalizers, ClusterIngressFinalizer)
			if err = r.client.Update(context.TODO(), ci); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to remove finalizer from clusteringress %s/%s: %v", ci.Namespace, ci.Name, err)
			}
		}
	} else {
		// Handle active ingress.
		if err := r.ensureRouterForIngress(ci); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to ensure clusteringress %s/%s: %v", ci.Namespace, ci.Name, err)
		}
	}

	return reconcile.Result{}, nil
}

// ensureRouterNamespace ensures all the necessary scaffolding exists for
// routers generally, including a namespace and all RBAC setup.
func (r *IngressReconciler) ensureRouterNamespace() error {
	cr, err := r.manifestFactory.RouterClusterRole()
	if err != nil {
		return fmt.Errorf("failed to build router cluster role: %v", err)
	}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: cr.Name}, cr)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get router cluster role %s: %v", cr.Name, err)
		}
		err = r.client.Create(context.TODO(), cr)
		if err == nil {
			logrus.Infof("created router cluster role %s", cr.Name)
		} else if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create router cluster role %s: %v", cr.Name, err)
		}
	}

	ns, err := r.manifestFactory.RouterNamespace()
	if err != nil {
		return fmt.Errorf("failed to build router namespace: %v", err)
	}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: ns.Name}, ns)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get router namespace %q: %v", ns.Name, err)
		}
		err = r.client.Create(context.TODO(), ns)
		if err == nil {
			logrus.Infof("created router namespace %s", ns.Name)
		} else if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create router namespace %s: %v", ns.Name, err)
		}
	}

	sa, err := r.manifestFactory.RouterServiceAccount()
	if err != nil {
		return fmt.Errorf("failed to build router service account: %v", err)
	}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: sa.Name, Namespace: sa.Namespace}, sa)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get router service account %s/%s: %v", sa.Namespace, sa.Name, err)
		}
		err = r.client.Create(context.TODO(), sa)
		if err == nil {
			logrus.Infof("created router service account %s/%s", sa.Namespace, sa.Name)
		} else if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create router service account %s/%s: %v", sa.Namespace, sa.Name, err)
		}
	}

	crb, err := r.manifestFactory.RouterClusterRoleBinding()
	if err != nil {
		return fmt.Errorf("failed to build router cluster role binding: %v", err)
	}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: crb.Name}, crb)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get router cluster role binding %s: %v", crb.Name, err)
		}
		err = r.client.Create(context.TODO(), crb)
		if err == nil {
			logrus.Infof("created router cluster role binding %s", crb.Name)
		} else if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create router cluster role binding %s: %v", crb.Name, err)
		}
	}

	return nil
}

// ensureRouterForIngress ensures all necessary router resources exist for a
// given clusteringress.
func (r *IngressReconciler) ensureRouterForIngress(ci *ingressv1alpha1.ClusterIngress) error {
	ds, err := r.manifestFactory.RouterDaemonSet(ci)
	if err != nil {
		return fmt.Errorf("failed to build router daemonset: %v", err)
	}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, ds)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get router daemonset %s/%s, %v", ds.Namespace, ds.Name, err)
		}
		err = r.client.Create(context.TODO(), ds)
		if err == nil {
			logrus.Infof("created router daemonset %s/%s", ds.Namespace, ds.Name)
		} else if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create router daemonset %s/%s: %v", ds.Namespace, ds.Name, err)
		}
	}

	if ci.Spec.HighAvailability != nil {
		switch ci.Spec.HighAvailability.Type {
		case ingressv1alpha1.CloudClusterIngressHA:
			service, err := r.manifestFactory.RouterServiceCloud(ci)
			if err != nil {
				return fmt.Errorf("failed to build router service: %v", err)
			}

			err = r.client.Get(context.TODO(), types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, service)
			if err != nil {
				if !errors.IsNotFound(err) {
					return fmt.Errorf("failed to get router service %s/%s, %v", service.Namespace, service.Name, err)
				}

				if err := controllerutil.SetControllerReference(ds, service, r.scheme); err != nil {
					return fmt.Errorf("failed to set owner reference on service %s/%s, %v", service.Namespace, service.Name, err)
				}
				err = r.client.Create(context.TODO(), service)
				if err == nil {
					logrus.Infof("created router service %s/%s", service.Namespace, service.Name)
				} else if !errors.IsAlreadyExists(err) {
					return fmt.Errorf("failed to create router service %s/%s: %v", service.Namespace, service.Name, err)
				}
			}
		}
	}

	return nil
}

// ensureRouterDeleted ensures that any router resources associated with the
// clusteringress are deleted.
func (r *IngressReconciler) ensureRouterDeleted(ci *ingressv1alpha1.ClusterIngress) error {
	ds, err := r.manifestFactory.RouterDaemonSet(ci)
	if err != nil {
		return fmt.Errorf("failed to build router daemonset for deletion: %v", err)
	}
	err = r.client.Delete(context.TODO(), ds)
	if !errors.IsNotFound(err) {
		return err
	}
	return nil
}
