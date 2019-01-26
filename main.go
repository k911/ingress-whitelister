package main

import (
	"flag"
	"fmt"
	"k8s.io/api/extensions/v1beta1"
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
	whiteLists                      map[string]string
	whiteListsMutex                 sync.RWMutex
	configMapIndexer                cache.Indexer
	ingressIndexer                  cache.Indexer
	configMapQueue                  workqueue.RateLimitingInterface
	ingressQueue                    workqueue.RateLimitingInterface
	configMapInformer               cache.Controller
	ingressInformer                 cache.Controller
	whiteListAnnotation             string
	whiteListSourceRangesAnnotation string
}

func NewController(
	configMapQueue workqueue.RateLimitingInterface,
	ingressQueue workqueue.RateLimitingInterface,
	configMapIndexer cache.Indexer,
	ingressIndexer cache.Indexer,
	configMapInformer cache.Controller,
	ingressInformer cache.Controller,
	whiteListAnnotation string,
	whiteListSourceRangesAnnotation string,
) *IngressWhitelisterController {
	return &IngressWhitelisterController{
		whiteLists:                      map[string]string{},
		whiteListsMutex:                 sync.RWMutex{},
		ingressInformer:                 ingressInformer,
		ingressIndexer:                  ingressIndexer,
		ingressQueue:                    ingressQueue,
		configMapInformer:               configMapInformer,
		configMapIndexer:                configMapIndexer,
		configMapQueue:                  configMapQueue,
		whiteListAnnotation:             whiteListAnnotation,
		whiteListSourceRangesAnnotation: whiteListSourceRangesAnnotation,
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

func (c *IngressWhitelisterController) updateIngress(key string) error {
	obj, exists, err := c.ingressIndexer.GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching Ingress with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		klog.Infof("Ingress %s does not exist anymore", key)
	} else {
		var ingress = obj.(*v1beta1.Ingress)
		klog.Infof("Sync/Add/Update for Ingress %s", ingress.GetName())
		needsUpdate := false
		// check if ingress has annotation
		if whitelist, ok := ingress.Annotations[c.whiteListAnnotation]; ok {
			klog.Infof("Ingress %s has whitelist: %s", ingress.GetName(), whitelist)
			klog.Infof("Whitelists locked for read")
			c.whiteListsMutex.RLock()
			if whitelistSourceRange, ok := c.whiteLists[whitelist]; ok {
				ingress.Annotations[c.whiteListSourceRangesAnnotation] = whitelistSourceRange
				needsUpdate = true
			} else {
				klog.Warningf("Ingress %s requests not existing whitelist: %s", ingress.GetName(), whitelist)
				delete(ingress.Annotations, c.whiteListSourceRangesAnnotation)
			}
			c.whiteListsMutex.RUnlock()
			klog.Infof("Whitelists unlocked for read")
		} else if _, ok := ingress.Annotations[c.whiteListSourceRangesAnnotation]; ok {
			klog.Infof("Ingress %s does not have whitelist anymore, deleting whitelist source ranges..", ingress.GetName())
			delete(ingress.Annotations, c.whiteListSourceRangesAnnotation)
			needsUpdate = true
		}

		if needsUpdate {
			klog.Infof("Updating ingress %s", ingress.GetName())
			err = c.ingressIndexer.Update(ingress)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *IngressWhitelisterController) updateWhiteListRanges(key string) error {
	obj, exists, err := c.configMapIndexer.GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching ConfigMap with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		klog.Infof("ConfigMap %s does not exist anymore", key)
	} else {
		var configmap = obj.(*v1.ConfigMap)
		klog.Infof("Sync/Add/Update for ConfigMap %s", configmap.GetName())

		c.whiteListsMutex.Lock()
		klog.Infof("Whitelists locked for write")
		c.whiteLists = configmap.Data
		for whitelist, ipRanges := range configmap.Data {
			klog.Infof("Whitelist \"%s\": %s", whitelist, ipRanges)
		}
		c.whiteListsMutex.Unlock()
		klog.Infof("Whitelists unlocked for write")

		// Force update ingresses
		err = c.ingressIndexer.Resync()
		if err != nil {
			return err
		}

	}
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
		// configMapQueue and the re-enqueue history, the key will be processed later again.
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
	defer c.configMapQueue.ShutDown()
	defer c.ingressQueue.ShutDown()
	klog.Info("Starting Ingress Whitelister controller")

	go c.configMapInformer.Run(stopCh)
	go c.ingressInformer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the configMapQueue is started
	if !cache.WaitForCacheSync(stopCh, c.configMapInformer.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for ConfigMap caches to sync"))
		return
	}
	if !cache.WaitForCacheSync(stopCh, c.ingressInformer.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for Ingress caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runConfigMapWorker, time.Second, stopCh)
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runIngressWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Stopping Ingress Whitelister controller")
}

func (c *IngressWhitelisterController) runIngressWorker() {
	klog.Info("Starting Ingress worker")
	for c.processQueueItem(c.ingressQueue, c.updateIngress, func(err error, key interface{}) {
		c.handleErrProcessingQueue("Ingress", c.ingressQueue, err, key)
	}) {
	}
	klog.Info("Stopping Ingress worker")
}

func (c *IngressWhitelisterController) runConfigMapWorker() {
	klog.Info("Starting ConfigMap worker")
	for c.processQueueItem(c.configMapQueue, c.updateWhiteListRanges, func(err error, key interface{}) {
		c.handleErrProcessingQueue("ConfigMap", c.configMapQueue, err, key)
	}) {
	}
	klog.Info("Stopping ConfigMap worker")
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
	flag.StringVar(configMap, "configmap", "default/ingress-whitelister", "ConfigMap to watch for whitelists source ranges")
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

func main() {
	var kubeconfig string
	var master string
	var configMap string
	var whiteListAnnotation string
	var whiteListSourceRangesAnnotation string

	err := initializeFlags(&kubeconfig, &master, &configMap, &whiteListAnnotation, &whiteListSourceRangesAnnotation)
	if err != nil {
		panic(err)
	}

	klog.Infof("--kubeconfig: %s", kubeconfig)
	klog.Infof("--master: %s", master)
	klog.Infof("--configmap: %s", configMap)
	klog.Infof("--whitelist-annotation: %s", whiteListAnnotation)
	klog.Infof("--whitelist-source-ranges-annotation: %s", whiteListSourceRangesAnnotation)

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

	// create the workqueue
	configMapQueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	ingressQueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	// Bind the workqueue to a cache with the help of an configMapInformer. This way we make sure that
	// whenever the cache is updated, the pod key is added to the workqueue.
	// Note that when we finally process the item from the workqueue, we might see a newer version
	// of the Pod than the version which was responsible for triggering the update.
	configMapIndexer, configMapInformer := cache.NewIndexerInformer(configMapWatcher, &v1.ConfigMap{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			klog.Infof("Add ConfigMap: %s", key)
			if err == nil {
				configMapQueue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			klog.Infof("Update ConfigMap: %s", key)
			if err == nil {
				configMapQueue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta configMapQueue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			klog.Infof("Delete ConfigMap: %s", key)
			if err == nil {
				configMapQueue.Add(key)
			}
		},
	}, cache.Indexers{})

	ingressIndexer, ingressInformer := cache.NewIndexerInformer(ingressWatcher, &v1beta1.Ingress{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			klog.Infof("Add Ingress: %s", key)
			if err == nil {
				ingressQueue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			klog.Infof("Update Ingress: %s", key)
			if err == nil {
				ingressQueue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			klog.Infof("Delete Ingress: %s", key)
			if err == nil {
				ingressQueue.Add(key)
			}
		},
	}, cache.Indexers{})

	controller := NewController(
		configMapQueue,
		ingressQueue,
		configMapIndexer,
		ingressIndexer,
		configMapInformer,
		ingressInformer,
		whiteListAnnotation,
		whiteListSourceRangesAnnotation,
	)

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	// Wait forever
	select {}
}
