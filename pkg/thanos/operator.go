// Copyright 2016 The prometheus-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"fmt"
	"time"

	"github.com/coreos/prometheus-operator/pkg/apis/monitoring"
	monitoringv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	monitoringclient "github.com/coreos/prometheus-operator/pkg/client/versioned"
	"github.com/coreos/prometheus-operator/pkg/k8sutil"
	"github.com/coreos/prometheus-operator/pkg/listwatch"
	prometheusoperator "github.com/coreos/prometheus-operator/pkg/prometheus"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	extensionsobj "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	resyncPeriod = 5 * time.Minute
)

// Operator manages life cycle of Thanos deployments and
// monitoring configurations.
type Operator struct {
	kclient   kubernetes.Interface
	mclient   monitoringclient.Interface
	crdclient apiextensionsclient.Interface
	logger    log.Logger

	thanosRulerInf cache.SharedIndexInformer
	queue          workqueue.RateLimitingInterface

	reconcileErrorsCounter *prometheus.CounterVec
	stsDeleteCreateCounter *prometheus.CounterVec
	triggerByCounter       *prometheus.CounterVec

	config Config
}

// Config defines configuration parameters for the Operator.
type Config struct {
	Host                   string
	TLSInsecure            bool
	TLSConfig              rest.TLSClientConfig
	ThanosDefaultBaseImage string
	Namespaces             prometheusoperator.Namespaces
	Labels                 prometheusoperator.Labels
	CrdKinds               monitoringv1.CrdKinds
	EnableValidation       bool
	LocalHost              string
	LogLevel               string
	LogFormat              string
	ManageCRDs             bool
	ThanosRulerSelector    string
}

// New creates a new controller.
func New(conf prometheusoperator.Config, logger log.Logger) (*Operator, error) {
	cfg, err := k8sutil.NewClusterConfig(conf.Host, conf.TLSInsecure, &conf.TLSConfig)
	if err != nil {
		return nil, errors.Wrap(err, "instantiating cluster config failed")
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "instantiating kubernetes client failed")
	}

	crdclient, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "instantiating apiextensions client failed")
	}

	mclient, err := monitoringclient.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "instantiating monitoring client failed")
	}

	if _, err := labels.Parse(conf.ThanosRulerSelector); err != nil {
		return nil, errors.Wrap(err, "can not parse thanos ruler selector value")
	}

	c := &Operator{
		kclient:   client,
		mclient:   mclient,
		crdclient: crdclient,
		logger:    logger,
		queue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "thanos"),
		config: Config{
			Host:                   conf.Host,
			TLSInsecure:            conf.TLSInsecure,
			TLSConfig:              conf.TLSConfig,
			ThanosDefaultBaseImage: conf.ThanosDefaultBaseImage,
			Namespaces:             conf.Namespaces,
			Labels:                 conf.Labels,
			CrdKinds:               conf.CrdKinds,
			EnableValidation:       conf.EnableValidation,
			LocalHost:              conf.LocalHost,
			LogLevel:               conf.LogLevel,
			LogFormat:              conf.LogFormat,
			ManageCRDs:             conf.ManageCRDs,
			ThanosRulerSelector:    conf.ThanosRulerSelector,
		},
	}

	c.thanosRulerInf = cache.NewSharedIndexInformer(
		listwatch.MultiNamespaceListerWatcher(c.logger, c.config.Namespaces.ThanosRulerAllowList, c.config.Namespaces.DenyList, func(namespace string) cache.ListerWatcher {
			return &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.LabelSelector = c.config.ThanosRulerSelector
					return mclient.MonitoringV1().ThanosRulers(namespace).List(options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.LabelSelector = c.config.ThanosRulerSelector
					return mclient.MonitoringV1().ThanosRulers(namespace).Watch(options)
				},
			}
		}),
		&monitoringv1.Prometheus{}, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	return c, nil
}

// RegisterMetrics registers Prometheus metrics on the given Prometheus
// registerer.
func (c *Operator) RegisterMetrics(r prometheus.Registerer, reconcileErrorsCounter *prometheus.CounterVec, triggerByCounter *prometheus.CounterVec, stsDeleteCreateCounter *prometheus.CounterVec) {
	c.reconcileErrorsCounter = reconcileErrorsCounter
	c.triggerByCounter = triggerByCounter
	c.stsDeleteCreateCounter = stsDeleteCreateCounter
	c.reconcileErrorsCounter.With(prometheus.Labels{}).Add(0)

	r.MustRegister(
		NewThanosRulerCollector(c.thanosRulerInf.GetStore()),
	)
}

// waitForCacheSync waits for the informers' caches to be synced.
func (c *Operator) waitForCacheSync(stopc <-chan struct{}) error {
	ok := true
	informers := []struct {
		name     string
		informer cache.SharedIndexInformer
	}{
		{"ThanosRuler", c.thanosRulerInf},
	}
	for _, inf := range informers {
		if !cache.WaitForCacheSync(stopc, inf.informer.HasSynced) {
			level.Error(c.logger).Log("msg", fmt.Sprintf("failed to sync %s cache", inf.name))
			ok = false
		} else {
			level.Debug(c.logger).Log("msg", fmt.Sprintf("successfully synced %s cache", inf.name))
		}
	}
	if !ok {
		return errors.New("failed to sync caches")
	}
	level.Info(c.logger).Log("msg", "successfully synced all caches")
	return nil
}

// addHandlers adds the eventhandlers to the informers.
func (c *Operator) addHandlers() {
	c.thanosRulerInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.handleThanosRulerAdd,
		DeleteFunc: c.handleThanosRulerDelete,
		UpdateFunc: c.handleThanosRulerUpdate,
	})
}

// Run the controller.
func (c *Operator) Run(stopc <-chan struct{}) error {
	defer c.queue.ShutDown()

	errChan := make(chan error)
	go func() {
		v, err := c.kclient.Discovery().ServerVersion()
		if err != nil {
			errChan <- errors.Wrap(err, "communicating with server failed")
			return
		}
		level.Info(c.logger).Log("msg", "connection established", "cluster-version", v)

		if c.config.ManageCRDs {
			if err := c.createCRDs(); err != nil {
				errChan <- errors.Wrap(err, "creating CRDs failed")
				return
			}
		}
		errChan <- nil
	}()

	select {
	case err := <-errChan:
		if err != nil {
			return err
		}
		level.Info(c.logger).Log("msg", "CRD API endpoints ready")
	case <-stopc:
		return nil
	}

	go c.worker()

	go c.thanosRulerInf.Run(stopc)
	if err := c.waitForCacheSync(stopc); err != nil {
		return err
	}
	c.addHandlers()

	<-stopc
	return nil
}

func (c *Operator) keyFunc(obj interface{}) (string, bool) {
	k, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		level.Error(c.logger).Log("msg", "creating key failed", "err", err)
		return k, false
	}
	return k, true
}

func (c *Operator) handleThanosRulerAdd(obj interface{}) {
	key, ok := c.keyFunc(obj)
	if !ok {
		return
	}

	level.Debug(c.logger).Log("msg", "ThanosRuler added", "key", key)
	c.triggerByCounter.WithLabelValues(monitoringv1.ThanosRulerKind, "add").Inc()
	c.enqueue(key)
}

func (c *Operator) handleThanosRulerDelete(obj interface{}) {
	key, ok := c.keyFunc(obj)
	if !ok {
		return
	}

	level.Debug(c.logger).Log("msg", "ThanosRuler deleted", "key", key)
	c.triggerByCounter.WithLabelValues(monitoringv1.ThanosRulerKind, "delete").Inc()
	c.enqueue(key)
}

func (c *Operator) handleThanosRulerUpdate(old, cur interface{}) {
	if old.(*monitoringv1.ThanosRuler).ResourceVersion == cur.(*monitoringv1.ThanosRuler).ResourceVersion {
		return
	}

	key, ok := c.keyFunc(cur)
	if !ok {
		return
	}

	level.Debug(c.logger).Log("msg", "ThanosRuler updated", "key", key)
	c.triggerByCounter.WithLabelValues(monitoringv1.ThanosRulerKind, "update").Inc()
	c.enqueue(key)
}

// enqueue adds a key to the queue. If obj is a key already it gets added
// directly. Otherwise, the key is extracted via keyFunc.
func (c *Operator) enqueue(obj interface{}) {
	if obj == nil {
		return
	}

	key, ok := obj.(string)
	if !ok {
		key, ok = c.keyFunc(obj)
		if !ok {
			return
		}
	}

	c.queue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. It enforces that the syncHandler is never invoked
// concurrently with the same key.
func (c *Operator) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *Operator) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.sync(key.(string))
	if err == nil {
		c.queue.Forget(key)
		return true
	}

	c.reconcileErrorsCounter.With(prometheus.Labels{}).Inc()
	utilruntime.HandleError(errors.Wrap(err, fmt.Sprintf("Sync %q failed", key)))
	c.queue.AddRateLimited(key)

	return true
}

func (c *Operator) sync(key string) error {
	obj, exists, err := c.thanosRulerInf.GetIndexer().GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		// Dependent resources are cleaned up by K8s via OwnerReferences
		return nil
	}

	p := obj.(*monitoringv1.ThanosRuler)
	p = p.DeepCopy()
	p.APIVersion = monitoringv1.SchemeGroupVersion.String()
	p.Kind = monitoringv1.ThanosRulerKind

	level.Info(c.logger).Log("msg", "sync thanos-ruler", "key", key)

	return nil
}

func (c *Operator) createCRDs() error {
	crds := []*extensionsobj.CustomResourceDefinition{
		k8sutil.NewCustomResourceDefinition(c.config.CrdKinds.ThanosRuler, monitoring.GroupName, c.config.Labels.LabelsMap, c.config.EnableValidation),
	}

	crdClient := c.crdclient.ApiextensionsV1().CustomResourceDefinitions()

	for _, crd := range crds {
		oldCRD, err := crdClient.Get(crd.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return errors.Wrapf(err, "getting CRD: %s", crd.Spec.Names.Kind)
		}
		if apierrors.IsNotFound(err) {
			if _, err := crdClient.Create(crd); err != nil {
				return errors.Wrapf(err, "creating CRD: %s", crd.Spec.Names.Kind)
			}
			level.Info(c.logger).Log("msg", "CRD created", "crd", crd.Spec.Names.Kind)
		}
		if err == nil {
			crd.ResourceVersion = oldCRD.ResourceVersion
			if _, err := crdClient.Update(crd); err != nil {
				return errors.Wrapf(err, "creating CRD: %s", crd.Spec.Names.Kind)
			}
			level.Info(c.logger).Log("msg", "CRD updated", "crd", crd.Spec.Names.Kind)
		}
	}

	crdListFuncs := []struct {
		name     string
		listFunc func(opts metav1.ListOptions) (runtime.Object, error)
	}{
		{
			monitoringv1.ThanosRulerKind,
			listwatch.MultiNamespaceListerWatcher(c.logger, c.config.Namespaces.ThanosRulerAllowList, c.config.Namespaces.DenyList, func(namespace string) cache.ListerWatcher {
				return &cache.ListWatch{
					ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
						return c.mclient.MonitoringV1().Prometheuses(namespace).List(options)
					},
				}
			}).List,
		},
		{
			monitoringv1.PrometheusRuleKind,
			listwatch.MultiNamespaceListerWatcher(c.logger, c.config.Namespaces.AllowList, c.config.Namespaces.DenyList, func(namespace string) cache.ListerWatcher {
				return &cache.ListWatch{
					ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
						return c.mclient.MonitoringV1().PrometheusRules(namespace).List(options)
					},
				}
			}).List,
		},
	}

	for _, crdListFunc := range crdListFuncs {
		err := k8sutil.WaitForCRDReady(crdListFunc.listFunc)
		if err != nil {
			return errors.Wrapf(err, "waiting for %v crd failed", crdListFunc.name)
		}
	}

	return nil
}
