/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sync"

	fastsvcv1 "github.com/YukioZzz/fastsvc/api/v1"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"

	faasv1 "github.com/Interstellarss/faas-share/pkg/apis/faasshare/v1"
	clientset "github.com/Interstellarss/faas-share/pkg/client/clientset/versioned"
	faasshareinformers "github.com/Interstellarss/faas-share/pkg/client/informers/externalversions"
	//faasshareinformers "github.com/Interstellarss/faas-share/pkg/client/informers/externalversions/faasshare/v1"
	faassharelisters "github.com/Interstellarss/faas-share/pkg/client/listers/faasshare/v1"
)

// FaSTSvcReconciler reconciles a FaSTSvc object
type FaSTSvcReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	promv1api      promv1.API
	sharepodLister faassharelisters.SharePodLister
}

func getRPS(funcname string, quota float64, partition int64) float64 {
	return 30.0
}
func (c *FaSTSvcReconciler) getNominalRPS() map[string]float64 {
	targetTotalRPS := make(map[string]float64)
	klog.Info("sharepodlist trying to list now!")
	sharePodList, _ := c.sharepodLister.List(labels.Everything())
	quota := 0.0
	partition := int64(100)
	klog.Info("Number of sharepod:", len(sharePodList))
	for _, sharepod := range sharePodList {
		funcname := sharepod.ObjectMeta.Labels["faas_function"]
		quota, _ = strconv.ParseFloat(sharepod.ObjectMeta.Annotations[faasv1.KubeShareResourceGPURequest], 64)
		partition, _ = strconv.ParseInt(sharepod.ObjectMeta.Annotations[faasv1.KubeShareResourceGPUPartition], 10, 64)
		replicas := float64(*sharepod.Spec.Replicas)
		klog.Info("Funcname:", funcname, " Quota:", quota, " Partition:", partition, " detected")
		if _, ok := targetTotalRPS[funcname]; !ok {
			targetTotalRPS[funcname] = 0.0
		}
		targetTotalRPS[funcname] += getRPS(funcname, quota, partition) * replicas
	}
	return targetTotalRPS
}

// reconcileServiceCRD reconciles the ML CRD containing the ML service under test
func (r *FaSTSvcReconciler) reconcileServiceCRD(instance *fastsvcv1.FaSTSvc, sharepod *faasv1.SharePod) (*faasv1.SharePod, error) {
	logger := log.FromContext(context.TODO())

	err := r.Get(context.TODO(), types.NamespacedName{Name: sharepod.GetName(), Namespace: sharepod.GetNamespace()}, sharepod)
	if err != nil {
		// If not created, create the service CRD
		if errors.IsNotFound(err) {
			klog.Info("Creating sharepod crd", "name", sharepod.GetName())
			err = r.Create(context.TODO(), sharepod)
			if err != nil {
				logger.Error(err, "Create service CRD error", "name", sharepod.GetName())
				return nil, err
			}
		} else {
			logger.Error(err, "Get sharepod CRD error", "name", sharepod.GetName())
			return nil, err
		}
	}
	return sharepod, nil
}

func (r *FaSTSvcReconciler) getDesiredCRDSpec(instance *fastsvcv1.FaSTSvc, nominalRPS float64, currentRPS float64) (*faasv1.SharePod, error) {
	// Prepare podTemplate and embed tunable parameters
	podSpec := corev1.PodSpec{}
	selector := metav1.LabelSelector{}
	if &instance.Spec != nil {
		instance.Spec.PodSpec.DeepCopyInto(&podSpec)
		instance.Spec.Selector.DeepCopyInto(&selector)
	}
	extendedAnnotations := make(map[string]string)
	extendedLabels := make(map[string]string)
	// Prepare k8s CRD
	quota := fmt.Sprintf("%0.2f", 0.1)
	partition := strconv.Itoa(30)
	extendedLabels["com.openfaas.scale.max"] = "1"
	extendedAnnotations["kubeshare/gpu_request"] = quota
	extendedAnnotations["kubeshare/gpu_limit"] = "1.0"
	extendedAnnotations["kubeshare/gpu_mem"] = "1073741824"
	extendedAnnotations["kubeshare/gpu_partition"] = partition
	extendedLabels["faas_function"] = instance.ObjectMeta.Name
	var fixedReplica_int32 int32 = int32(1)

	sharepod := &faasv1.SharePod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        instance.ObjectMeta.Name + "-q" + quota + "-p" + partition,
			Namespace:   "faas-share-fn",
			Labels:      extendedLabels,
			Annotations: extendedAnnotations,
		},
		Spec: faasv1.SharePodSpec{
			Selector: &selector,
			PodSpec:  podSpec,
			Replicas: &fixedReplica_int32,
		},
	}
	// ToDo: SetControllerReference here is useless, as the controller delete svc upon trial completion
	// Add owner reference to the service so that it could be GC
	//if err := controllerutil.SetControllerReference(instance, sharepod, r.Scheme); err != nil {
	//	return nil, err
	//}
	return sharepod, nil
}

//func (r *FaSTSvcReconciler) updateSharepodsConfig(instance *fastsvcv1.FaSTSvc, nominalRPS float64) ([]*faasv1.SharePod, error) {
//
//
//}

func (r *FaSTSvcReconciler) bypassReconcile() {
	// Get real RPS
	//loop each req, get its rps via prometheus rate
	// get all FaSTSvc resources
	ticker := time.NewTicker(1 * time.Second) // run every 5 seconds
	defer ticker.Stop()

	for range ticker.C {
		logger := log.FromContext(context.TODO())

		ctx := context.TODO()
		var allFaSTSvcs fastsvcv1.FaSTSvcList
		if err := r.List(ctx, &allFaSTSvcs); err != nil {
			return
		}
		nominalRPSMAP := r.getNominalRPS()

		// loop through each FaSTSvc resource
		for _, svc := range allFaSTSvcs.Items {
			// get the function name from the metadata name of the FaSTSvc resource
			funcName := svc.ObjectMeta.Name
			// make a Prometheus query to get the RPS of the function
			query := fmt.Sprintf("rate(gateway_function_invocation_total{function_name='%s.%s'}[10s])", funcName, svc.ObjectMeta.Namespace)
			klog.Info("Query:", query)
			currentRPSvec, _, err := r.promv1api.Query(ctx, query, time.Now())
			currentRPS := float64(0.0)
			nominalRPS := float64(0.0)
			if err != nil {
				continue
			} else {
				if currentRPSvec.(model.Vector).Len() != 0 {
					klog.Info("current rps vec:", currentRPSvec)
					currentRPS = float64(currentRPSvec.(model.Vector)[0].Value)
				}
			}
			// process the Prometheus query result to get the RPS value
			klog.Info("current rps: ", currentRPS)
			//newSharePods := updateSharepodsConfig(&svc, currentRPS)
			if nmnRPS, ok := nominalRPSMAP[funcName]; !ok {
				klog.Info("Does not have nominal rps for ", funcName, ", set as zero")
			} else {
				nominalRPS = nmnRPS
			}
			// do something with the RPS value
			klog.Info("Nominal rps", nominalRPS)
			desiredCRD, err := r.getDesiredCRDSpec(&svc, nominalRPS, currentRPS)
			if err != nil {
				logger.Error(err, "Target CRD construction error")
				continue
			}
			_, err = r.reconcileServiceCRD(&svc, desiredCRD)
			if err != nil {
				logger.Error(err, "Reconcile CRD error")
				continue
			}
		}
	}
}

var once sync.Once

//+kubebuilder:rbac:groups=fastsvc.fastsvc.tum,resources=fastsvcs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=fastsvc.fastsvc.tum,resources=fastsvcs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=fastsvc.fastsvc.tum,resources=fastsvcs/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the FaSTSvc object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *FaSTSvcReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	once.Do(func() {
		go r.bypassReconcile()
	})
	return ctrl.Result{}, nil
}

func getSharePodLister(client clientset.Interface, namespace string, stopCh chan struct{}) faassharelisters.SharePodLister {
	// create a shared informer factory for the FaasShare API group
	informerFactory := faasshareinformers.NewSharedInformerFactoryWithOptions(
		client,
		0,
		faasshareinformers.WithNamespace(namespace),
	)

	// retrieve the shared informer for SharePods
	sharePodInformer := informerFactory.Kubeshare().V1().SharePods().Informer()
	informerFactory.Start(stopCh)
	if !cache.WaitForCacheSync(stopCh, sharePodInformer.HasSynced) {
		return nil
	}

	// create a lister for SharePods using the shared informer's indexers
	sharePodLister := faassharelisters.NewSharePodLister(sharePodInformer.GetIndexer())

	return sharePodLister
}

// SetupWithManager sets up the controller with the Manager.
func (r *FaSTSvcReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Create a Prometheus API client
	promClient, err := api.NewClient(api.Config{
		Address: "http://prometheus.faas-share.svc.cluster.local:9090",
	})
	if err != nil {
		return err
	}
	r.promv1api = promv1.NewAPI(promClient)
	client, _ := clientset.NewForConfig(ctrl.GetConfigOrDie())
	stopCh := make(chan struct{})
	r.sharepodLister = getSharePodLister(client, "faas-share-fn", stopCh)
	faasv1.AddToScheme(r.Scheme)

	return ctrl.NewControllerManagedBy(mgr).
		For(&fastsvcv1.FaSTSvc{}).
		Complete(r)
}
