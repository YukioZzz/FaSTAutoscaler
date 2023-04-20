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
	"math"
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
	"regexp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sync"

	fastsvcv1 "github.com/YukioZzz/fastsvc/api/v1"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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
func (r *FaSTSvcReconciler) reconcileServiceCRD(instance *fastsvcv1.FaSTSvc, sharepods []*faasv1.SharePod) error {
	logger := log.FromContext(context.TODO())

	for _, sharepod := range sharepods {
		existed := &faasv1.SharePod{}
		err := r.Get(context.TODO(), types.NamespacedName{Name: sharepod.GetName(), Namespace: sharepod.GetNamespace()}, existed)
		if err != nil {
			// If not created, create the service CRD
			if errors.IsNotFound(err) {
				klog.Info("Creating sharepod crd ", sharepod.GetName())
				err = r.Create(context.TODO(), sharepod)
				if err != nil {
					logger.Error(err, "Create service CRD error", "name", sharepod.GetName())
					return err
				}
			} else {
				logger.Error(err, "Get sharepod CRD error", "name", sharepod.GetName())
				return err
			}
		}
		replica := int32(*sharepod.Spec.Replicas)
		klog.Info("Updating sharepod crd ", sharepod.GetName(), " with Replica ", replica)
		existed.Spec.Replicas = &replica
		err = r.Update(context.TODO(), existed)
		if err != nil {
			logger.Error(err, "failed to update CRD")
			return err
		}
	}
	return nil
}

func (r *FaSTSvcReconciler) configs2sharepods(instance *fastsvcv1.FaSTSvc, configs []*SharePodConfig) ([]*faasv1.SharePod, error) {
	sharepodList := make([]*faasv1.SharePod, 0)
	for _, config := range configs {
		klog.Info("Creating New sharepods with config ", getKeyName(config.Quota, config.Partition), " and Replica: ", config.Replica)
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
		quota := fmt.Sprintf("%0.2f", float64(config.Quota)/100.0)
		partition := strconv.Itoa(int(config.Partition))
		extendedLabels["com.openfaas.scale.min"] = strconv.Itoa(int(config.Replica))
		extendedLabels["com.openfaas.scale.max"] = strconv.Itoa(int(config.Replica))
		extendedAnnotations["kubeshare/gpu_request"] = quota
		extendedAnnotations["kubeshare/gpu_limit"] = "1.0"
		extendedAnnotations["kubeshare/gpu_mem"] = "1073741824"
		extendedAnnotations["kubeshare/gpu_partition"] = partition
		extendedLabels["faas_function"] = instance.ObjectMeta.Name
		fixedReplica_int32 := int32(config.Replica)

		sharepod := &faasv1.SharePod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        instance.ObjectMeta.Name + getKeyName(config.Quota, config.Partition),
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
		if err := controllerutil.SetControllerReference(instance, sharepod, r.Scheme); err != nil {
			klog.Info("Error setting ownerref")
			return nil, err
		}
		sharepodList = append(sharepodList, sharepod)
	}
	return sharepodList, nil
}

func getKeyName(quota int64, partition int64) string {
	return "-q" + strconv.Itoa(int(quota)) + "-p" + strconv.Itoa(int(partition))
}
func parseFromKeyName(key string) (int, int) {
	r := regexp.MustCompile(`-q(\d+)-p(\d+)`)
	match := r.FindStringSubmatch(key)
	if match == nil {
		return 0, 0
	}
	quota, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, 0
	}
	partition, err := strconv.Atoi(match[2])
	if err != nil {
		return 0, 0
	}
	return quota, partition
}
func (c *FaSTSvcReconciler) getNominalRPSList(funcname string) (map[string]int64, float64) {
	klog.Info("sharepodLister trying to list now!")
	selector := labels.SelectorFromSet(labels.Set{"faas_function": funcname})
	sharePodList, _ := c.sharepodLister.List(selector)
	quota := 0.0
	targetTotalRPS := 0.0
	partition := int64(100)
	klog.Info("Number of sharepod:", len(sharePodList))
	nominalRPSList := make(map[string]int64)
	for _, sharepod := range sharePodList {
		quota, _ = strconv.ParseFloat(sharepod.ObjectMeta.Annotations[faasv1.KubeShareResourceGPURequest], 64)
		partition, _ = strconv.ParseInt(sharepod.ObjectMeta.Annotations[faasv1.KubeShareResourceGPUPartition], 10, 64)
		replicas := int64(*sharepod.Spec.Replicas)
		klog.Info("Funcname:", funcname, " Quota:", quota, " Partition:", partition, " detected")
		nominalRPSList[getKeyName(int64(quota*100), partition)] = replicas
		targetTotalRPS += getRPS(funcname, quota, partition) * float64(replicas)
	}
	klog.Info("totalRPS of ", funcname, ":", targetTotalRPS)
	return nominalRPSList, targetTotalRPS
}

type SharePodConfig struct {
	Quota     int64 //percentage
	Partition int64 //percentage
	Replica   int64
}

func getEfficientConfig() (SharePodConfig, float64) {
	return SharePodConfig{30, 12, 1}, 50.0
}
func (r *FaSTSvcReconciler) schedule(request float64) []*SharePodConfig {
	var configs []*SharePodConfig
	// scheduleSaturate
	configs_eff, rps_eff := getEfficientConfig()
	configs_eff.Replica = int64(request / rps_eff)
	residue_loads := math.Mod(request, rps_eff)

	// scheduleResidule
	if residue_loads > 0.0 {
		configs_ideal, _ := getEfficientConfig() //argminp(ProcessRatei[p] − ri) where ProcessRatei[p] > ri
		if configs_eff.Quota == configs_ideal.Quota && configs_eff.Partition == configs_ideal.Partition {
			configs_eff.Replica += 1
		} else {
			configs = append(configs, &configs_ideal)
		}
	}

	configs = append(configs, &configs_eff)
	return configs
}

func (r *FaSTSvcReconciler) scaleDownSharepod(funcname string, replica int64) {
	existed := &faasv1.SharePod{}
	err := r.Get(context.TODO(), types.NamespacedName{Name: funcname, Namespace: "faas-share-fn"}, existed)
	if err != nil {
		// If not created, report error
		klog.Error("scale down error with getter fail")
		return
	}
	replicas := int32(replica)
	klog.Info("Updating sharepod crd ", funcname, " with Replica ", replicas)
	existed.Spec.Replicas = &replicas
	existed.ObjectMeta.Labels["com.openfaas.scale.max"] = strconv.Itoa(int(replicas))
	existed.ObjectMeta.Labels["com.openfaas.scale.min"] = strconv.Itoa(int(replicas))
	err = r.Update(context.TODO(), existed)
	if err != nil {
		klog.Error("failed to update CRD when scaling down")
		return
	}

}

func (r *FaSTSvcReconciler) getDesiredCRDSpec(instance *fastsvcv1.FaSTSvc, currentRPS float64, pastRPS float64) []*faasv1.SharePod {
	// compare nominal and current real rps
	// if too much, then scale down
	nominalRPSList, totalRPS := r.getNominalRPSList(instance.ObjectMeta.Name)
	// scale up first
	newRequests := currentRPS + 1.0 - totalRPS

	preidct := currentRPS - pastRPS

	if newRequests > 0 || preidct >= 30 {
		klog.Info("scaling up now")
		configsList := r.schedule(newRequests)
		for _, config := range configsList {
			config.Replica += nominalRPSList[getKeyName(config.Quota, config.Partition)]
		}
		sharepods, _ := r.configs2sharepods(instance, configsList)
		return sharepods
	} else if newRequests < 0 {
		// else, scale down. Traverse from small to big and pop out smallest
		newRequests = newRequests * -1
		for key, replica := range nominalRPSList {
			quota, partition := parseFromKeyName(key)
			rpsUnit := getRPS(instance.ObjectMeta.Name, float64(quota)/100.0, int64(partition))
			n := int64(newRequests / rpsUnit)
			if replica > n {
				replica = replica - n
				newRequests = newRequests - float64(n)*rpsUnit
			} else {
				replica = 0
				newRequests = newRequests - float64(replica)*rpsUnit
			}
			r.scaleDownSharepod(instance.ObjectMeta.Name+key, replica)
		}
	}

	return nil
}

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

		// loop through each FaSTSvc resource
		for _, svc := range allFaSTSvcs.Items {
			// get the function name from the metadata name of the FaSTSvc resource
			funcName := svc.ObjectMeta.Name
			// make a Prometheus query to get the RPS of the function
			query := fmt.Sprintf("rate(gateway_function_invocation_total{function_name='%s.%s'}[10s])", funcName, svc.ObjectMeta.Namespace)
			klog.Info("Query:", query)
			currentRPSvec, _, err := r.promv1api.Query(ctx, query, time.Now())
			currentRPS := float64(0.0)

			if err != nil {
				continue
			} else {
				if currentRPSvec.(model.Vector).Len() != 0 {
					klog.Info("current rps vec:", currentRPSvec)
					currentRPS = float64(currentRPSvec.(model.Vector)[0].Value)
				}
			}

			pastRPSvec, _, err := r.promv1api.Query(ctx, query, time.Now().Add(-30*time.Second))
			pastRPS := float64(0.0)

			if err != nil {
				continue
			} else {
				if pastRPSvec.(model.Vector).Len() != 0 {
					klog.Info("past 30s rps vec:", pastRPS)
					pastRPS = float64(pastRPSvec.(model.Vector)[0].Value)
				}
			}

			// process the Prometheus query result to get the RPS value
			klog.Info("current rps: ", currentRPS)
			desiredCRDs := r.getDesiredCRDSpec(&svc, currentRPS, pastRPS)
			err = r.reconcileServiceCRD(&svc, desiredCRDs)
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
