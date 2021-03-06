package main

import (
	"flag"
	"fmt"
	"k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/klog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
)

type IngressWhitelisterController struct {
	Clientset                       *kubernetes.Clientset
	WatchedIngresses                map[string]string
	WatchedIngressesMutex           sync.RWMutex
	AvailableWhitelists             map[string]string
	AvailableWhitelistsMutex        sync.RWMutex
	ConfigMapIndexer                cache.Indexer
	IngressIndexer                  cache.Indexer
	ConfigMapQueue                  workqueue.RateLimitingInterface
	IngressQueue                    workqueue.RateLimitingInterface
	ConfigMapInformer               cache.Controller
	IngressInformer                 cache.Controller
	WhitelistAnnotation             string
	WhitelistSourceRangesAnnotation string
}

func NewController(
	clientset *kubernetes.Clientset,
	configMapQueue workqueue.RateLimitingInterface,
	ingressQueue workqueue.RateLimitingInterface,
	configMapIndexer cache.Indexer,
	ingressIndexer cache.Indexer,
	configMapInformer cache.Controller,
	ingressInformer cache.Controller,
	whitelistAnnotation string,
	whitelistSourceRangesAnnotation string,
) *IngressWhitelisterController {
	return &IngressWhitelisterController{
		Clientset:                       clientset,
		WatchedIngresses:                map[string]string{},
		WatchedIngressesMutex:           sync.RWMutex{},
		AvailableWhitelists:             map[string]string{},
		AvailableWhitelistsMutex:        sync.RWMutex{},
		IngressInformer:                 ingressInformer,
		IngressIndexer:                  ingressIndexer,
		IngressQueue:                    ingressQueue,
		ConfigMapInformer:               configMapInformer,
		ConfigMapIndexer:                configMapIndexer,
		ConfigMapQueue:                  configMapQueue,
		WhitelistAnnotation:             whitelistAnnotation,
		WhitelistSourceRangesAnnotation: whitelistSourceRangesAnnotation,
	}
}

func (c *IngressWhitelisterController) processQueueItem(
	queue workqueue.RateLimitingInterface,
	handler func(string) error,
	errorHandler func(error, interface{}),
) bool {
	key, quit := queue.Get()
	if quit {
		return false
	}
	defer queue.Done(key)

	// Invoke the method containing the business logic
	err := handler(key.(string))

	// Handle the error if something went wrong during the execution of the business logic
	errorHandler(err, key)
	return true
}

func (c *IngressWhitelisterController) unwatchIngress(key string) {
	c.WatchedIngressesMutex.RLock()
	_, watched := c.WatchedIngresses[key]
	c.WatchedIngressesMutex.RUnlock()

	if watched {
		c.WatchedIngressesMutex.Lock()
		delete(c.WatchedIngresses, key)
		c.WatchedIngressesMutex.Unlock()
		klog.Infof("Finished watching ingress: %s", key)
	}
}

func (c *IngressWhitelisterController) cleanIngress(ingress *v1beta1.Ingress) {
	klog.Infof("Cleaning ingress: %s/%s", ingress.GetNamespace(), ingress.GetName())

	delete(ingress.Annotations, c.WhitelistSourceRangesAnnotation)
	client := c.Clientset.ExtensionsV1beta1().Ingresses(ingress.GetNamespace())
	_, updateErr := client.Update(ingress)

	if updateErr != nil {
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			result, getErr := client.Get(ingress.GetName(), metav1.GetOptions{})
			if getErr != nil {
				klog.Errorf("Failed to get latest version of ingress: %v", getErr)
			}

			delete(ingress.Annotations, c.WhitelistSourceRangesAnnotation)
			_, updateErr := client.Update(result)
			return updateErr
		})

		if retryErr != nil {
			klog.Errorf("Cleaning ingress failed: %v", retryErr)
		}
	}
}

func (c *IngressWhitelisterController) whitelistIngress(whitelistSourceRange string, ingress *v1beta1.Ingress) {
	klog.Infof("Updating ingress: %s/%s", ingress.GetNamespace(), ingress.GetName())

	ingress.Annotations[c.WhitelistSourceRangesAnnotation] = whitelistSourceRange
	client := c.Clientset.ExtensionsV1beta1().Ingresses(ingress.GetNamespace())
	_, updateErr := client.Update(ingress)

	if updateErr != nil {
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			result, getErr := client.Get(ingress.GetName(), metav1.GetOptions{})
			if getErr != nil {
				klog.Errorf("Failed to get latest version of ingress: %v", getErr)
			}

			ingress.Annotations[c.WhitelistSourceRangesAnnotation] = whitelistSourceRange
			_, updateErr := client.Update(result)
			return updateErr
		})

		if retryErr != nil {
			klog.Errorf("Whitelisting ingress failed: %v", retryErr)
		}
	}
}

func (c *IngressWhitelisterController) processIngress(key string) error {
	obj, exists, err := c.IngressIndexer.GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching ingress with key \"%s\" from store failed with %v", key, err)
		return err
	}

	if !exists {
		c.unwatchIngress(key)
	} else {
		var ingress = obj.(*v1beta1.Ingress)
		klog.V(2).Infof("Processing ingress %s/%s", ingress.GetNamespace(), ingress.GetName())

		currentWhitelist, hasWhitelist := ingress.Annotations[c.WhitelistAnnotation]
		currentWhitelistSourceRange, hasWhitelistSourceRange := ingress.Annotations[c.WhitelistSourceRangesAnnotation]

		if !hasWhitelist {
			if hasWhitelistSourceRange {
				c.cleanIngress(ingress)
			}
			c.unwatchIngress(key)
			return nil
		}

		c.AvailableWhitelistsMutex.RLock()
		realWhitelistSourceRange, whitelistExists := c.AvailableWhitelists[currentWhitelist]
		c.AvailableWhitelistsMutex.RUnlock()

		c.WatchedIngressesMutex.RLock()
		cachedWhitelist, watched := c.WatchedIngresses[key]
		c.WatchedIngressesMutex.RUnlock()

		whitelistChanged := watched && currentWhitelist != cachedWhitelist

		if !watched || whitelistChanged {
			klog.Infof("Assigning ingress \"%s/%s\" to whitelist: %s", ingress.GetNamespace(), ingress.GetName(), currentWhitelist)
			c.WatchedIngressesMutex.Lock()
			c.WatchedIngresses[key] = currentWhitelist
			c.WatchedIngressesMutex.Unlock()
		}

		if !whitelistExists {
			klog.Errorf("Ingress \"%s/%s\" references unspecified whitelist: %s", ingress.GetNamespace(), ingress.GetName(), currentWhitelist)

			if hasWhitelistSourceRange {
				c.cleanIngress(ingress)
			}
			return nil
		}

		whitelistSourceRangeChanged := !hasWhitelistSourceRange || currentWhitelistSourceRange != realWhitelistSourceRange

		if whitelistChanged || whitelistSourceRangeChanged {
			c.whitelistIngress(realWhitelistSourceRange, ingress)
		}
	}
	return nil
}

func (c *IngressWhitelisterController) processWhitelists(key string) error {
	obj, exists, err := c.ConfigMapIndexer.GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching configmap with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		klog.Info("Cleaning whitelists as configmap does not exist anymore")
		c.AvailableWhitelistsMutex.Lock()
		c.AvailableWhitelists = map[string]string{}
		c.AvailableWhitelistsMutex.Unlock()
	} else {
		var configmap = obj.(*v1.ConfigMap)
		klog.Infof("Updating whitelists from configmap: %s", configmap.GetName())

		c.AvailableWhitelistsMutex.Lock()
		c.AvailableWhitelists = configmap.Data
		for whitelist, ipRanges := range configmap.Data {
			klog.Infof("Whitelist \"%s\": %s", whitelist, ipRanges)
		}
		c.AvailableWhitelistsMutex.Unlock()
	}

	c.WatchedIngressesMutex.RLock()
	for ingressKey := range c.WatchedIngresses {
		klog.V(2).Infof("Touching ingress: %s", ingressKey)
		c.IngressQueue.Add(ingressKey)
	}
	c.WatchedIngressesMutex.RUnlock()

	return nil
}

// handleErrProcessingQueue checks if an error happened and makes sure we will retry later.
func (c *IngressWhitelisterController) handleErrProcessingQueue(
	queueName string,
	queue workqueue.RateLimitingInterface,
	err error,
	key interface{},
) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if queue.NumRequeues(key) < 5 {
		klog.Infof("Error syncing key %v from %s queue: %v", key, queueName, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// ConfigMapQueue and the re-enqueue history, the key will be processed later again.
		queue.AddRateLimited(key)
		return
	}

	queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping key %q out of the %s queue: %v", key, queueName, err)
}

func (c *IngressWhitelisterController) Run(threadiness int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.ConfigMapQueue.ShutDown()
	defer c.IngressQueue.ShutDown()
	klog.Info("Starting ingress whitelister controller")

	go c.ConfigMapInformer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the ConfigMapQueue is started
	if !cache.WaitForCacheSync(stopCh, c.ConfigMapInformer.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for configmap caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runConfigMapWorker, time.Second, stopCh)
	}

	go c.IngressInformer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.IngressInformer.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for ingress caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runIngressWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Stopping ingress whitelister controller")
}

func (c *IngressWhitelisterController) runIngressWorker() {
	klog.V(2).Info("Starting ingress whitelister worker")
	for c.processQueueItem(c.IngressQueue, c.processIngress, func(err error, key interface{}) {
		c.handleErrProcessingQueue("Ingress", c.IngressQueue, err, key)
	}) {
	}
	klog.Info("Stopping ingress whitelister worker")
}

func (c *IngressWhitelisterController) runConfigMapWorker() {
	klog.V(2).Info("Starting whitelist watcher worker")
	for c.processQueueItem(c.ConfigMapQueue, c.processWhitelists, func(err error, key interface{}) {
		c.handleErrProcessingQueue("ConfigMap", c.ConfigMapQueue, err, key)
	}) {
	}
	klog.Info("Stopping whitelist watcher worker")
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func initializeFlags(
	kubeconfig *string,
	master *string,
	configMap *string,
	whiteListAnnotation *string,
	whiteListSourceRangesAnnotation *string,
) error {
	if home := homeDir(); home != "" {
		flag.StringVar(kubeconfig, "kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		flag.StringVar(kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.StringVar(master, "master", "", "master url")
	flag.StringVar(configMap, "configmap", "default/ingress-whitelister", "ConfigMap to watch for AvailableWhitelists source ranges")
	flag.StringVar(whiteListAnnotation, "whitelist-annotation", "ingress-whitelister.ingress.kubernetes.io/whitelist-name", "Ingress annotation to watch for whitelist change")
	flag.StringVar(whiteListSourceRangesAnnotation, "whitelist-source-ranges-annotation", "ingress-whitelister.ingress.kubernetes.io/whitelist-source-range", "ingress annotation to append with contents of whitelist source range")
	err := flag.Set("logtostderr", "true")
	if err != nil {
		return err
	}

	flag.Parse()
	klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(klogFlags)

	// Sync the glog and klog flags.
	flag.CommandLine.VisitAll(func(f1 *flag.Flag) {
		f2 := klogFlags.Lookup(f1.Name)
		if f2 != nil {
			value := f1.Value.String()
			_ = f2.Value.Set(value)
		}
	})

	return nil
}

func NewConfigMapWatcher(clientset *kubernetes.Clientset, configmap string) *cache.ListWatch {
	split := strings.Split(configmap, "/")

	return cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(), "configmaps", split[0], fields.Set{
			"metadata.name": split[1],
		}.AsSelector())
}

func NewIngressWatcher(clientset *kubernetes.Clientset) *cache.ListWatch {
	return cache.NewListWatchFromClient(
		clientset.ExtensionsV1beta1().RESTClient(), "ingresses", v1.NamespaceAll, fields.Everything())
}

func NewQueueAddingEventHandler(queue workqueue.RateLimitingInterface) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				klog.V(2).Infof("Key \"%s\" added", key)
				queue.Add(key)
			} else {
				klog.Warning(err)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				klog.V(2).Infof("Key \"%s\" updated", key)
				queue.Add(key)
			} else {
				klog.Warning(err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				klog.V(2).Infof("Key \"%s\" deleted", key)
				queue.Add(key)
			} else {
				klog.Warning(err)
			}
		},
	}
}

func main() {
	var kubeconfig string
	var master string
	var configMap string
	var whitelistAnnotation string
	var whitelistSourceRangesAnnotation string

	err := initializeFlags(&kubeconfig, &master, &configMap, &whitelistAnnotation, &whitelistSourceRangesAnnotation)
	if err != nil {
		panic(err)
	}

	klog.Infof("--kubeconfig: %s", kubeconfig)
	klog.Infof("--master: %s", master)
	klog.Infof("--configmap: %s", configMap)
	klog.Infof("--whitelist-annotation: %s", whitelistAnnotation)
	klog.Infof("--whitelist-source-ranges-annotation: %s", whitelistSourceRangesAnnotation)

	// creates the connection
	config, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	configMapWatcher := NewConfigMapWatcher(clientset, configMap)
	ingressWatcher := NewIngressWatcher(clientset)

	configMapQueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	ingressQueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	configMapIndexer, configMapInformer := cache.NewIndexerInformer(configMapWatcher, &v1.ConfigMap{}, 0, NewQueueAddingEventHandler(configMapQueue), cache.Indexers{})
	ingressIndexer, ingressInformer := cache.NewIndexerInformer(ingressWatcher, &v1beta1.Ingress{}, 0, NewQueueAddingEventHandler(ingressQueue), cache.Indexers{})

	controller := NewController(
		clientset,
		configMapQueue,
		ingressQueue,
		configMapIndexer,
		ingressIndexer,
		configMapInformer,
		ingressInformer,
		whitelistAnnotation,
		whitelistSourceRangesAnnotation,
	)

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	// Wait forever
	select {}
}
