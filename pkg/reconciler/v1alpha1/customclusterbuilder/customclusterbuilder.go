package customclusterbuilder

import (
	"context"

	"github.com/google/go-containerregistry/pkg/authn"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"
	"knative.dev/pkg/controller"

	"github.com/pivotal/kpack/pkg/apis/build/v1alpha1"
	experimentalV1alpha1 "github.com/pivotal/kpack/pkg/apis/experimental/v1alpha1"
	"github.com/pivotal/kpack/pkg/client/clientset/versioned"
	v1alpha1informers "github.com/pivotal/kpack/pkg/client/informers/externalversions/experimental/v1alpha1"
	v1alpha1Listers "github.com/pivotal/kpack/pkg/client/listers/experimental/v1alpha1"
	"github.com/pivotal/kpack/pkg/cnb"
	"github.com/pivotal/kpack/pkg/reconciler"
	"github.com/pivotal/kpack/pkg/registry"
	"github.com/pivotal/kpack/pkg/tracker"
)

const (
	ReconcilerName = "CustomBuilders"
	Kind           = "CustomBuilder"
)

type BuilderCreator interface {
	CreateBuilder(keychain authn.Keychain, store cnb.Store, spec experimentalV1alpha1.CustomBuilderSpec) (v1alpha1.BuilderRecord, error)
}

func NewController(
	opt reconciler.Options,
	informer v1alpha1informers.CustomClusterBuilderInformer,
	builderCreator BuilderCreator,
	keychainFactory registry.KeychainFactory,
	storeInformer v1alpha1informers.StoreInformer) *controller.Impl {
	c := &Reconciler{
		Client:                     opt.Client,
		CustomClusterBuilderLister: informer.Lister(),
		BuilderCreator:             builderCreator,
		KeychainFactory:            keychainFactory,
		StoreLister:                storeInformer.Lister(),
	}
	impl := controller.NewImpl(c, opt.Logger, ReconcilerName)
	informer.Informer().AddEventHandler(reconciler.Handler(impl.Enqueue))

	c.Tracker = tracker.New(impl.EnqueueKey, opt.TrackerResyncPeriod())
	storeInformer.Informer().AddEventHandler(reconciler.Handler(c.Tracker.OnChanged))

	return impl
}

type Reconciler struct {
	Client                     versioned.Interface
	CustomClusterBuilderLister v1alpha1Listers.CustomClusterBuilderLister
	BuilderCreator             BuilderCreator
	KeychainFactory            registry.KeychainFactory
	Tracker                    *tracker.Tracker
	StoreLister                v1alpha1Listers.StoreLister
}

func (c *Reconciler) Reconcile(ctx context.Context, key string) error {
	_, builderName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	customBuilder, err := c.CustomClusterBuilderLister.Get(builderName)
	if k8serrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}

	customBuilder = customBuilder.DeepCopy()

	builderRecord, creationError := c.reconcileCustomBuilder(customBuilder)
	if creationError != nil {
		customBuilder.Status.ErrorCreate(creationError)

		err := c.updateStatus(customBuilder)
		if err != nil {
			return err
		}

		return controller.NewPermanentError(creationError)
	}

	customBuilder.Status.BuilderRecord(builderRecord)
	return c.updateStatus(customBuilder)
}

func (c *Reconciler) reconcileCustomBuilder(customBuilder *experimentalV1alpha1.CustomClusterBuilder) (v1alpha1.BuilderRecord, error) {
	keychain, err := c.KeychainFactory.KeychainForSecretRef(registry.SecretRef{
		ServiceAccount: customBuilder.Spec.ServiceAccountRef.Name,
		Namespace:      customBuilder.Spec.ServiceAccountRef.Namespace,
	})
	if err != nil {
		return v1alpha1.BuilderRecord{}, err
	}

	store, err := c.StoreLister.Get(customBuilder.Spec.Store.Name)
	if err != nil {
		return v1alpha1.BuilderRecord{}, err
	}

	buildPackageStore := &cnb.BuildpackRetriever{
		Keychain: nil, // TODO: this should be a keychain created from the store spec not the builder spec
		Client:   &registry.Client{},
		Store:    store,
	}

	return c.BuilderCreator.CreateBuilder(keychain, buildPackageStore, customBuilder.Spec.CustomBuilderSpec)
}

func (c *Reconciler) updateStatus(desired *experimentalV1alpha1.CustomClusterBuilder) error {
	desired.Status.ObservedGeneration = desired.Generation

	original, err := c.CustomClusterBuilderLister.Get(desired.Name)
	if err != nil {
		return err
	}

	if equality.Semantic.DeepEqual(desired.Status, original.Status) {
		return nil
	}

	_, err = c.Client.ExperimentalV1alpha1().CustomClusterBuilders().UpdateStatus(desired)
	return err
}