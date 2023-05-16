// SPDX-FileCopyrightText: 2022 SAP SE or an SAP affiliate company and Open Component Model contributors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	eventv1 "github.com/fluxcd/pkg/apis/event/v1beta1"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	rreconcile "github.com/fluxcd/pkg/runtime/reconcile"
	"github.com/mandelsoft/vfs/pkg/osfs"
	"github.com/mandelsoft/vfs/pkg/projectionfs"
	"github.com/tetratelabs/wazero"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kuberecorder "k8s.io/client-go/tools/record"
	"k8s.io/kubectl/pkg/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	ocmv1 "github.com/open-component-model/ocm/pkg/contexts/ocm"

	"github.com/open-component-model/ocm-controller/api/v1alpha1"
	"github.com/open-component-model/ocm-controller/pkg/cache"
	"github.com/open-component-model/ocm-controller/pkg/component"
	"github.com/open-component-model/ocm-controller/pkg/event"
	"github.com/open-component-model/ocm-controller/pkg/ocm"
	"github.com/open-component-model/ocm-controller/pkg/snapshot"
	"github.com/open-component-model/ocm/pkg/common"
	"github.com/open-component-model/ocm/pkg/contexts/ocm/accessmethods/localblob"
	"github.com/open-component-model/ocm/pkg/contexts/ocm/accessmethods/ociartifact"
	"github.com/open-component-model/ocm/pkg/contexts/ocm/accessmethods/ociblob"
	ocmmetav1 "github.com/open-component-model/ocm/pkg/contexts/ocm/compdesc/meta/v1"
	"github.com/open-component-model/ocm/pkg/contexts/ocm/download/handlers/dirtree"

	ocmreg "github.com/open-component-model/ocm/pkg/contexts/ocm/repositories/ocireg"
	"github.com/wapc/wapc-go"
	wazeroEngine "github.com/wapc/wapc-go/engines/wazero"
)

// ResourceReconciler reconciles a Resource object
type ResourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	kuberecorder.EventRecorder
	OCMClient      ocm.Contract
	Cache          cache.Cache
	SnapshotWriter snapshot.Writer
}

// +kubebuilder:rbac:groups=delivery.ocm.software,resources=resources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=resources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=resources/finalizers,verbs=update

// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	const (
		resourceKey = ".metadata.resource"
	)

	if err := mgr.GetFieldIndexer().IndexField(context.TODO(), &v1alpha1.Resource{}, resourceKey, func(rawObj client.Object) []string {
		res := rawObj.(*v1alpha1.Resource)
		return []string{res.Spec.SourceRef.Name}

	}); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Resource{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&source.Kind{Type: &v1alpha1.ComponentVersion{}},
			handler.EnqueueRequestsFromMapFunc(r.findObjects(resourceKey)),
			builder.WithPredicates(ComponentVersionChangedPredicate{}),
		).
		Complete(r)
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logger := log.FromContext(ctx).WithName("resource-controller")

	obj := &v1alpha1.Resource{}
	if err = r.Client.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get resource object: %w", err)
	}

	if obj.Spec.Suspend {
		logger.Info("resource object suspended")
		return result, nil
	}

	var patchHelper *patch.Helper
	patchHelper, err = patch.NewHelper(obj, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create patch helper: %w", err)
	}

	// Always attempt to patch the object and status after each reconciliation.
	defer func() {
		// Patching has not been set up, or the controller errored earlier.
		if patchHelper == nil {
			return
		}

		if condition := conditions.Get(obj, meta.StalledCondition); condition != nil && condition.Status == metav1.ConditionTrue {
			conditions.Delete(obj, meta.ReconcilingCondition)
		}

		// Check if it's a successful reconciliation.
		// We don't set Requeue in case of error, so we can safely check for Requeue.
		if result.RequeueAfter == obj.GetRequeueAfter() && !result.Requeue && err == nil {
			// Remove the reconciling condition if it's set.
			conditions.Delete(obj, meta.ReconcilingCondition)

			// Set the return err as the ready failure message is the resource is not ready, but also not reconciling or stalled.
			if ready := conditions.Get(obj, meta.ReadyCondition); ready != nil && ready.Status == metav1.ConditionFalse && !conditions.IsStalled(obj) {
				err = errors.New(conditions.GetMessage(obj, meta.ReadyCondition))
			}
		}

		// If still reconciling then reconciliation did not succeed, set to ProgressingWithRetry to
		// indicate that reconciliation will be retried.
		if conditions.IsReconciling(obj) {
			reconciling := conditions.Get(obj, meta.ReconcilingCondition)
			reconciling.Reason = meta.ProgressingWithRetryReason
			conditions.Set(obj, reconciling)
		}

		// If not reconciling or stalled than mark Ready=True
		if !conditions.IsReconciling(obj) && !conditions.IsStalled(obj) &&
			err == nil && result.RequeueAfter == obj.GetRequeueAfter() {
			conditions.MarkTrue(obj, meta.ReadyCondition, meta.SucceededReason, "Reconciliation success")
			event.New(r.EventRecorder, obj, eventv1.EventSeverityInfo, "Reconciliation success", nil)
		}

		// Set status observed generation option if the object is stalled or ready.
		if conditions.IsStalled(obj) || conditions.IsReady(obj) {
			obj.Status.ObservedGeneration = obj.Generation
			event.New(r.EventRecorder, obj, eventv1.EventSeverityInfo, fmt.Sprintf("Reconciliation finished, next run in %s", obj.GetRequeueAfter()),
				map[string]string{v1alpha1.GroupVersion.Group + "/resource_version": obj.Status.LastAppliedResourceVersion})
		}

		if perr := patchHelper.Patch(ctx, obj); perr != nil {
			err = errors.Join(err, perr)
		}
	}()

	// if the snapshot name has not been generated then
	// generate, patch the status and requeue
	if obj.GetSnapshotName() == "" {
		name, err := snapshot.GenerateSnapshotName(obj.GetName())
		if err != nil {
			return ctrl.Result{}, err
		}
		obj.Status.SnapshotName = name
		return ctrl.Result{Requeue: true}, nil
	}

	return r.reconcile(ctx, obj)
}

func (r *ResourceReconciler) reconcile(ctx context.Context, obj *v1alpha1.Resource) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("resource-controller")

	rreconcile.ProgressiveStatus(false, obj, meta.ProgressingReason, "reconciliation in progress")

	if obj.Generation != obj.Status.ObservedGeneration {
		rreconcile.ProgressiveStatus(false, obj, meta.ProgressingReason,
			"processing object: new generation %d -> %d", obj.Status.ObservedGeneration, obj.Generation)
	}

	if obj.Spec.SourceRef.Namespace == "" {
		obj.Spec.SourceRef.Namespace = obj.GetNamespace()
	}

	conditions.Delete(obj, meta.StalledCondition)

	componentVersion := &v1alpha1.ComponentVersion{}
	if err := r.Get(ctx, obj.Spec.SourceRef.GetObjectKey(), componentVersion); err != nil {
		err = fmt.Errorf("failed to get component version: %w", err)
		conditions.MarkFalse(obj, meta.ReadyCondition, v1alpha1.GetResourceFailedReason, err.Error())
		event.New(r.EventRecorder, obj, eventv1.EventSeverityError, err.Error(), nil)
		return ctrl.Result{}, err
	}

	octx, err := r.OCMClient.CreateAuthenticatedOCMContext(ctx, componentVersion)
	if err != nil {
		err = fmt.Errorf("failed to create authenticated client: %w", err)
		conditions.MarkFalse(obj, meta.ReadyCondition, v1alpha1.AuthenticatedContextCreationFailedReason, err.Error())
	}

	cv, err := r.OCMClient.GetComponentVersion(ctx, octx, componentVersion)
	if err != nil {
		return ctrl.Result{}, err
	}

	res, err := cv.GetResource(ocmmetav1.NewIdentity(obj.Spec.SourceRef.ResourceRef.Name))
	if err != nil {
		return ctrl.Result{}, err
	}

	dir, err := os.MkdirTemp("", "wasm-tmp-")
	if err != nil {
		return ctrl.Result{}, err
	}
	defer os.RemoveAll(dir)

	tmpfs, err := projectionfs.New(osfs.New(), dir)
	if err != nil {
		os.Remove(dir)
	}

	_, _, err = dirtree.New().Download(common.NewPrinter(os.Stdout), res, "", tmpfs)
	if err != nil {
		return ctrl.Result{}, err
	}

	filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		decode := scheme.Codecs.UniversalDeserializer().Decode
		obj, _, err := decode(data, nil, nil)
		b, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		return os.WriteFile(p, b, fs.ModeType)
	})

	engine := wazeroEngine.Engine()

	for _, md := range obj.Spec.Middleware {
		mdRepo, err := octx.RepositoryForSpec(ocmreg.NewRepositorySpec(md.Registry, nil))
		if err != nil {
			return ctrl.Result{}, err
		}
		defer mdRepo.Close()

		component := strings.Split(md.Component, ":")
		middlewareCV, err := mdRepo.LookupComponentVersion(component[0], component[1])
		if err != nil {
			return ctrl.Result{}, err
		}
		defer middlewareCV.Close()

		res, err := middlewareCV.GetResource(ocmmetav1.NewIdentity(md.Name))
		if err != nil {
			return ctrl.Result{}, err
		}

		meth, err := res.AccessMethod()
		if err != nil {
			return ctrl.Result{}, err
		}

		data, err := meth.Get()
		if err != nil {
			return ctrl.Result{}, err
		}

		module, err := engine.New(ctx, makeHost(cv, dir), data, &wapc.ModuleConfig{
			Logger: wapc.PrintlnLogger,
			Stdout: os.Stdout,
			Stderr: os.Stderr,
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		defer module.Close(ctx)

		module.(*wazeroEngine.Module).WithConfig(func(config wazero.ModuleConfig) wazero.ModuleConfig {
			conf := wazero.NewFSConfig().WithDirMount(dir, "/data")
			return config.WithFSConfig(conf).WithSysWalltime()
		})

		instance, err := module.Instantiate(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		defer instance.Close(ctx)

		_, err = instance.Invoke(ctx, "handler", md.Values.Raw)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	version := "latest"
	if obj.Spec.SourceRef.GetVersion() != "" {
		version = obj.Spec.SourceRef.GetVersion()
	}

	// This is important because THIS is the actual component for our resource. If we used ComponentVersion in the
	// below identity, that would be the top-level component instead of the component that this resource belongs to.
	componentDescriptor, err := component.GetComponentDescriptor(ctx, r.Client, obj.GetReferencePath(), componentVersion.Status.ComponentDescriptor)
	if err != nil {
		err = fmt.Errorf("failed to get component descriptor for resource: %w", err)
		conditions.MarkFalse(obj, meta.ReadyCondition, v1alpha1.GetComponentDescriptorFailedReason, err.Error())
		event.New(r.EventRecorder, obj, eventv1.EventSeverityError, err.Error(), nil)
		return ctrl.Result{}, err
	}

	if componentDescriptor == nil {
		err := fmt.Errorf("couldn't find component descriptor for reference '%s' or any root components", obj.GetReferencePath())
		conditions.MarkFalse(obj, meta.ReadyCondition, v1alpha1.ComponentDescriptorNotFoundReason, err.Error())
		// Mark stalled because we can't do anything until the component descriptor is available. Likely requires some sort of manual intervention.
		conditions.MarkStalled(obj, v1alpha1.ComponentDescriptorNotFoundReason, err.Error())
		event.New(r.EventRecorder, obj, eventv1.EventSeverityError, err.Error(), nil)

		return ctrl.Result{}, nil
	}

	conditions.Delete(obj, meta.StalledCondition)

	identity := ocmmetav1.Identity{
		v1alpha1.ComponentNameKey:    componentDescriptor.Name,
		v1alpha1.ComponentVersionKey: componentDescriptor.Spec.Version,
		v1alpha1.ResourceNameKey:     obj.Spec.SourceRef.ResourceRef.Name,
		v1alpha1.ResourceVersionKey:  version,
	}
	for k, v := range obj.Spec.SourceRef.ResourceRef.ExtraIdentity {
		identity[k] = v
	}

	if obj.GetSnapshotName() == "" {
		return ctrl.Result{}, fmt.Errorf("snapshot name should not be empty")
	}

	_, err = r.SnapshotWriter.Write(ctx, obj, dir, identity)
	if err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("successfully pushed snapshot for resource", "resource", obj.Spec.SourceRef.Name)

	obj.Status.LastAppliedResourceVersion = obj.Spec.SourceRef.GetVersion()
	obj.Status.ObservedGeneration = obj.GetGeneration()
	obj.Status.LastAppliedComponentVersion = componentVersion.Status.ReconciledVersion

	logger.Info("successfully reconciled resource", "name", obj.GetName())

	// Remove any stale Ready condition, most likely False, set above. Its value
	// is derived from the overall result of the reconciliation in the deferred
	// block at the very end.
	conditions.Delete(obj, meta.ReadyCondition)

	return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, nil
}

// this function will enqueue a reconciliation for any snapshot which is referenced
// in the .spec.sourceRef or spec.configRef field of a Localization
func (r *ResourceReconciler) findObjects(key string) func(client.Object) []reconcile.Request {
	return func(obj client.Object) []reconcile.Request {
		resources := &v1alpha1.ResourceList{}
		if err := r.List(context.TODO(), resources, &client.ListOptions{
			FieldSelector: fields.OneTermEqualSelector(key, obj.GetName()),
			Namespace:     obj.GetNamespace(),
		}); err != nil {
			return []reconcile.Request{}
		}

		requests := make([]reconcile.Request, len(resources.Items))
		for i, item := range resources.Items {
			// if the observedgeneration is -1
			// then the object has not been initialised yet
			// so we should not trigger a reconcilation for sourceRef/configRefs
			if item.Status.ObservedGeneration != -1 {
				requests[i] = reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      item.GetName(),
						Namespace: item.GetNamespace(),
					},
				}
			}
		}

		return requests
	}
}

func makeHost(cv ocmv1.ComponentVersionAccess, dir string) func(ctx context.Context, binding, namespace, operation string, payload []byte) ([]byte, error) {
	return func(ctx context.Context, binding, namespace, operation string, payload []byte) ([]byte, error) {
		if binding != "ocm.software" {
			return nil, errors.New("unrecognised binding")
		}
		switch namespace {
		case "get":
			switch operation {
			case "resource":
				res, err := cv.GetResource(ocmmetav1.NewIdentity(string(payload)))
				if err != nil {
					return nil, err
				}

				ref, err := getReference(cv.GetContext(), res)
				if err != nil {
					return nil, err
				}

				return []byte(ref), nil
			}
		}
		return nil, errors.New("unrecognised namespace")
	}
}

func getReference(octx ocmv1.Context, res ocmv1.ResourceAccess) (string, error) {
	accSpec, err := res.Access()
	if err != nil {
		return "", err
	}

	var (
		ref    string
		refErr error
	)

	for ref == "" && refErr == nil {
		switch x := accSpec.(type) {
		case *ociartifact.AccessSpec:
			ref = x.ImageReference
		case *ociblob.AccessSpec:
			ref = fmt.Sprintf("%s@%s", x.Reference, x.Digest)
		case *localblob.AccessSpec:
			if x.GlobalAccess == nil {
				refErr = errors.New("cannot determine image digest")
			} else {
				accSpec, refErr = octx.AccessSpecForSpec(x.GlobalAccess)
			}
		default:
			refErr = errors.New("cannot determine access spec type")
		}
	}

	return ref, nil
}
