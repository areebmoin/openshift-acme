package acme

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"gopkg.in/yaml.v2"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	"github.com/tnozicka/openshift-acme/pkg/api"
	"github.com/tnozicka/openshift-acme/pkg/helpers"
	kubeinformers "github.com/tnozicka/openshift-acme/pkg/machinery/informers/kube"
)

const (
	ControllerName = "openshift-acme-acme-account-controller"
)

var (
	KeyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc
)

var once sync.Once

func acceptTerms(tosURL string) bool {
	once.Do(func() {
		klog.Infof("By continuing running this program you agree to the CA's Terms of Service (%s). If you do not agree exit the program immediately!", tosURL)
	})

	return true
}

type AccountController struct {
	kubeClient                 kubernetes.Interface
	kubeInformersForNamespaces kubeinformers.Interface

	cachesToSync []cache.InformerSynced

	recorder record.EventRecorder

	queue workqueue.RateLimitingInterface
}

func NewAccountController(
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces kubeinformers.Interface,
) *AccountController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	ac := &AccountController{
		kubeClient:                 kubeClient,
		kubeInformersForNamespaces: kubeInformersForNamespaces,

		recorder: eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: ControllerName}),

		queue: workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
	}

	for _, namespace := range kubeInformersForNamespaces.Namespaces() {
		informers := kubeInformersForNamespaces.InformersFor(namespace)

		informers.Core().V1().ConfigMaps().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    ac.addConfigMap,
			UpdateFunc: ac.updateConfigMap,
			DeleteFunc: ac.deleteConfigMap,
		})
		ac.cachesToSync = append(ac.cachesToSync, informers.Core().V1().ConfigMaps().Informer().HasSynced)

		informers.Core().V1().Secrets().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			// Controller is only provisioning new secret if it is missing so it only cares to reconcile deletes.
			DeleteFunc: ac.deleteSecret,
		})
		ac.cachesToSync = append(ac.cachesToSync, informers.Core().V1().Secrets().Informer().HasSynced)
	}

	return ac
}

func (ac *AccountController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ac.queue.ShutDown()

	klog.Info("Starting Account controller")
	defer klog.Info("Shutting down Account controller")

	if !cache.WaitForCacheSync(stopCh, ac.cachesToSync...) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	klog.Info("Account controller informer caches synced")

	for i := 0; i < workers; i++ {
		go wait.Until(ac.runWorker, time.Second, stopCh)
	}

	<-stopCh
}

func (ac *AccountController) runWorker() {
	for ac.processNextItem() {
	}
}

func (ac *AccountController) processNextItem() bool {
	key, quit := ac.queue.Get()
	if quit {
		return false
	}
	defer ac.queue.Done(key)

	err := ac.sync(key.(string))

	if err == nil {
		ac.queue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))
	ac.queue.AddRateLimited(key)

	return true
}

func (ac *AccountController) enqueueAccount(cm *corev1.ConfigMap) {
	key, err := KeyFunc(cm)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for account object: %w", err))
		return
	}

	ac.queue.Add(key)
}

func (ac *AccountController) addConfigMap(obj interface{}) {
	cm := obj.(*corev1.ConfigMap)

	if !api.AccountLabelSet.AsSelector().Matches(labels.Set(cm.Labels)) {
		klog.V(5).Infof("Skipping ConfigMap %s/%s UID=%s RV=%s", cm.Namespace, cm.Name, cm.UID, cm.ResourceVersion)
		return
	}

	klog.V(4).Infof("Adding ConfigMap %s/%s UID=%s RV=%s", cm.Namespace, cm.Name, cm.UID, cm.ResourceVersion)
	ac.enqueueAccount(cm)
}

func (ac *AccountController) updateConfigMap(old, cur interface{}) {
	oldConfigMap := old.(*corev1.ConfigMap)
	newConfigMap := cur.(*corev1.ConfigMap)

	if !api.AccountLabelSet.AsSelector().Matches(labels.Set(newConfigMap.Labels)) {
		klog.V(5).Infof("Skipping ConfigMap %s/%s UID=%s RV=%s", newConfigMap.Namespace, newConfigMap.Name, newConfigMap.UID, newConfigMap.ResourceVersion)
		return
	}

	klog.V(4).Infof("Updating ConfigMap from %s/%s UID=%s RV=%s to %s/%s UID=%s,RV=%s",
		oldConfigMap.Namespace, oldConfigMap.Name, oldConfigMap.UID, oldConfigMap.ResourceVersion,
		newConfigMap.Namespace, newConfigMap.Name, newConfigMap.UID, newConfigMap.ResourceVersion)

	ac.enqueueAccount(newConfigMap)
}

func (ac *AccountController) deleteConfigMap(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("object is not a ConfigMap neither tombstone: %T", obj))
			return
		}
		cm, ok = tombstone.Obj.(*corev1.ConfigMap)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a ConfigMap %T", obj))
			return
		}
	}

	if !api.AccountLabelSet.AsSelector().Matches(labels.Set(cm.Labels)) {
		klog.V(5).Infof("Skipping ConfigMap %s/%s UID=%s RV=%s", cm.Namespace, cm.Name, cm.UID, cm.ResourceVersion)
		return
	}

	klog.V(4).Infof("Deleting ConfigMap %s/%s UID=%s RV=%s", cm.Namespace, cm.Name, cm.UID, cm.ResourceVersion)
	ac.enqueueAccount(cm)
}

func (ac *AccountController) deleteSecret(obj interface{}) {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %T", obj))
			return
		}
		secret, ok = tombstone.Obj.(*corev1.Secret)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Secret %T", obj))
			return
		}
	}

	allConfigMaps, err := ac.kubeInformersForNamespaces.InformersForOrGlobal(secret.Namespace).Core().V1().ConfigMaps().Lister().ConfigMaps(secret.Namespace).List(api.AccountLabelSet.AsSelector())
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	for _, cm := range allConfigMaps {
		certIssuerData, ok := cm.Data[api.CertIssuerDataKey]
		if !ok {
			klog.Warning("ConfigMap %s/%s is matching CertIssuer selectors %q but missing key %q", cm.Namespace, cm.Name, api.AccountLabelSet, api.CertIssuerDataKey)
			continue
		}

		certIssuer := &api.CertIssuer{}
		err := yaml.Unmarshal([]byte(certIssuerData), certIssuer)
		if err != nil {
			klog.Warning("ConfigMap %s/%s is matching CertIssuer selectors %q but contains invalid object: %w", cm.Namespace, cm.Name, api.AccountLabelSet, err)
			continue
		}

		switch certIssuer.Type {
		case api.CertIssuerTypeAcme:
			if certIssuer.AcmeCertIssuer.AccountCredentialsSecretName == secret.Name {
				ac.enqueueAccount(cm)
			}
		default:
			continue
		}
	}
}

func (ac *AccountController) sync(key string) error {
	klog.V(4).Infof("Started syncing Account %q", key)
	defer func() {
		klog.V(4).Infof("Finished syncing Account %q", key)
	}()

	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(err)
		return err
	}

	objReadOnly, exists, err := ac.kubeInformersForNamespaces.InformersForOrGlobal(namespace).Core().V1().ConfigMaps().Informer().GetIndexer().GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching object with key %s from store failed with %w", key, err)
		return err
	}

	if !exists {
		klog.V(4).Infof("ConfigMap %s does not exist anymore\n", key)
		return nil
	}

	cmReadOnly := objReadOnly.(*corev1.ConfigMap)

	// Don't act on objects that are being deleted.
	if cmReadOnly.DeletionTimestamp != nil {
		return nil
	}

	certIssuerData, ok := cmReadOnly.Data[api.CertIssuerDataKey]
	if !ok {
		return fmt.Errorf("configmap %s is matching CertIssuer selectors %q but missing key %q", key, api.AccountLabelSet, api.CertIssuerDataKey)
	}

	certIssuer := &api.CertIssuer{}
	err = yaml.Unmarshal([]byte(certIssuerData), certIssuer)
	if err != nil {
		return fmt.Errorf("configmap %s is matching CertIssuer selectors %q but contains invalid object: %w", key, api.AccountLabelSet, err)
	}

	switch certIssuer.Type {
	case api.CertIssuerTypeAcme:
		break
	default:
		return nil
	}

	// TODO: Validate account fields

	acmeIssuer := certIssuer.AcmeCertIssuer

	client := &acme.Client{
		DirectoryURL: acmeIssuer.DirectoryUrl,
		UserAgent:    "github.com/tnozicka/openshift-acme",
	}
	account := &acme.Account{
		Contact: acmeIssuer.Account.Contacts,
	}

	if len(acmeIssuer.AccountCredentialsSecretName) == 0 {
		acmeIssuer.AccountCredentialsSecretName = cmReadOnly.Name
	}

	secret, err := ac.kubeInformersForNamespaces.InformersForOrGlobal(cmReadOnly.Namespace).Core().V1().Secrets().Lister().Secrets(cmReadOnly.Namespace).Get(acmeIssuer.AccountCredentialsSecretName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	} else if apierrors.IsNotFound(err) {
		// Register new account
		privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			return err
		}
		client.Key = privateKey

		keyPem := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
		})

		registerCtx, registerCtxCancel := context.WithTimeout(context.TODO(), 15*time.Second)
		defer registerCtxCancel()
		account, err = client.Register(registerCtx, account, acceptTerms)
		if err != nil {
			return err
		}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: acmeIssuer.AccountCredentialsSecretName,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				corev1.TLSPrivateKeyKey: keyPem,
			},
		}
		secret, err = ac.kubeClient.CoreV1().Secrets(cmReadOnly.Namespace).Create(secret)
		if err != nil {
			return err
		}
	} else {
		client.Key, err = helpers.PrivateKeyFromSecret(secret)
		if err != nil {
			return err
		}

		// Todo sign statuses with client.Key.Sign so the can't be modified externally

		accountHash := hashAccount(acmeIssuer.Account)

		if reflect.DeepEqual(accountHash, []byte(acmeIssuer.Account.Status.Hash)) {
			// Update the acme account to reflect user changes
			account.Contact = acmeIssuer.Account.Contacts

			updateCtx, updateCtxCancel := context.WithTimeout(context.TODO(), 15*time.Second)
			defer updateCtxCancel()
			account, err = client.UpdateReg(updateCtx, account)
			if err != nil {
				return err
			}
		} else if len(acmeIssuer.Account.Status.URI) == 0 {
			getRegCtx, getRegCtxCancel := context.WithTimeout(context.TODO(), 15*time.Second)
			defer getRegCtxCancel()
			// url argument is not needed for RFC 8555 compliant CAs
			account, err = client.GetReg(getRegCtx, "")
			if err != nil {
				return err
			}
		}
	}

	acmeIssuer.Account.Status.URI = account.URI
	acmeIssuer.Account.Contacts = account.Contact
	acmeIssuer.Account.Status.OrdersURL = account.OrdersURL
	acmeIssuer.Account.Status.AccountStatus = account.Status
	acmeIssuer.Account.Status.Hash = fmt.Sprint(hashAccount(acmeIssuer.Account))

	cm := cmReadOnly.DeepCopy()
	certIssuerBytes, err := yaml.Marshal(certIssuer)
	if err != nil {
		return fmt.Errorf("configmap %s is matching CertIssuer selectors %q but contains invalid object: %w", key, api.AccountLabelSet, err)
	}

	cm.Data[api.CertIssuerDataKey] = string(certIssuerBytes)

	if apiequality.Semantic.DeepEqual(cmReadOnly, cm) {
		return nil
	}

	_, err = ac.kubeClient.CoreV1().ConfigMaps(cmReadOnly.Namespace).Update(cm)
	if err != nil {
		return err
	}

	return nil
}

func hashAccount(account api.AcmeAccount) [64]byte {
	return sha512.Sum512([]byte(fmt.Sprint(account.Contacts)))
}
