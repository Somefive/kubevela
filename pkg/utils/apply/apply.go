/*
Copyright 2021 The KubeVela Authors.

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

package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/crossplane/crossplane-runtime/pkg/fieldpath"
	"github.com/kubevela/pkg/util/k8s"
	"github.com/kubevela/pkg/util/k8s/apply"
	velapatch "github.com/kubevela/pkg/util/k8s/patch"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/controller/utils"
	"github.com/oam-dev/kubevela/pkg/features"
	"github.com/oam-dev/kubevela/pkg/oam"
	"github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/pkg/utils/common"
)

const (
	// LabelRenderHash is the label that record the hash value of the rendering resource.
	LabelRenderHash = "oam.dev/render-hash"
)

type applyAction struct {
	takeOver         bool
	readOnly         bool
	isShared         bool
	skipUpdate       bool
	updateAnnotation bool
	dryRun           bool
	updateStrategy   v1alpha1.ResourceUpdateStrategy
}

func (in *applyAction) ApplyToCreate(opt *client.CreateOptions) {
	if in.dryRun {
		client.DryRunAll.ApplyToCreate(opt)
	}
}

func (in *applyAction) ApplyToUpdate(opt *client.UpdateOptions) {
	if in.dryRun {
		client.DryRunAll.ApplyToUpdate(opt)
	}
}

func (in *applyAction) ApplyToPatch(opt *client.PatchOptions) {
	if in.dryRun {
		client.DryRunAll.ApplyToPatch(opt)
	}
}

// ApplyOption is called before applying state to the object.
// ApplyOption is still called even if the object does NOT exist.
// If the object does not exist, `existing` will be assigned as `nil`.
// nolint
type ApplyOption func(act *applyAction, existing, desired client.Object) error

// trimLastAppliedConfigurationForSpecialResources will filter special object that can reduce the record for "app.oam.dev/last-applied-configuration" annotation.
func trimLastAppliedConfigurationForSpecialResources(desired client.Object) bool {
	if desired == nil {
		return false
	}
	gvk := desired.GetObjectKind().GroupVersionKind()
	gp, kd := gvk.Group, gvk.Kind
	if gp == "" {
		// group is empty means it's Kubernetes core API, we won't record annotation for Secret and Configmap
		if kd == "Secret" || kd == "ConfigMap" || kd == "CustomResourceDefinition" {
			return false
		}
		if _, ok := desired.(*corev1.ConfigMap); ok {
			return false
		}
		if _, ok := desired.(*corev1.Secret); ok {
			return false
		}
		if _, ok := desired.(*v1.CustomResourceDefinition); ok {
			return false
		}
	}
	ann := desired.GetAnnotations()
	if ann != nil {
		lac := ann[oam.AnnotationLastAppliedConfig]
		if lac == "-" || lac == "skip" {
			return false
		}
	}
	return true
}

func needRecreate(recreateFields []string, existing, desired client.Object) (bool, error) {
	if len(recreateFields) == 0 {
		return false, nil
	}
	_existing, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(existing)
	_desired, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	flag := false
	for _, field := range recreateFields {
		ve, err := fieldpath.Pave(_existing).GetValue(field)
		if err != nil {
			return false, fmt.Errorf("unable to get path %s from existing object: %w", field, err)
		}
		vd, err := fieldpath.Pave(_desired).GetValue(field)
		if err != nil {
			return false, fmt.Errorf("unable to get path %s from desired object: %w", field, err)
		}
		if !reflect.DeepEqual(ve, vd) {
			flag = true
		}
	}
	return flag, nil
}

// Apply applies new state to an object or create it if not exist
func Apply(ctx context.Context, c client.Client, desired client.Object, ao ...ApplyOption) error {
	_, err := generateRenderHash(desired)
	if err != nil {
		return err
	}
	applyAct := &applyAction{updateAnnotation: trimLastAppliedConfigurationForSpecialResources(desired)}
	ac := &apply.Client{Client: c}
	existing, err := createOrGetExisting(ctx, applyAct, ac, desired, ao...)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}

	// the object already exists, apply new state
	if err := executeApplyOptions(applyAct, existing, desired, ao); err != nil {
		return err
	}

	if applyAct.skipUpdate {
		klog.V(4).InfoS("skip update", "resource", klog.KObj(desired))
		return nil
	}

	strategy := applyAct.updateStrategy
	if strategy.Op == "" {
		if utilfeature.DefaultMutableFeatureGate.Enabled(features.ApplyResourceByReplace) && isUpdatableResource(desired) {
			strategy.Op = v1alpha1.ResourceUpdateStrategyReplace
		} else {
			strategy.Op = v1alpha1.ResourceUpdateStrategyPatch
		}
	}

	shouldRecreate, err := needRecreate(strategy.RecreateFields, existing, desired)
	if err != nil {
		return fmt.Errorf("failed to evaluate recreateFields: %w", err)
	}
	if shouldRecreate {
		klog.V(4).InfoS("recreating object", "resource", klog.KObj(desired))
		if applyAct.dryRun { // recreate does not support dryrun
			return nil
		}
		if existing.GetDeletionTimestamp() == nil { // check if recreation needed
			if err = ac.Delete(ctx, existing); err != nil {
				return errors.Wrap(err, "cannot delete object")
			}
		}
		return errors.Wrap(ac.Create(ctx, desired), "cannot recreate object")
	}

	switch strategy.Op {
	case v1alpha1.ResourceUpdateStrategyReplace:
		klog.V(4).InfoS("replacing object", "resource", klog.KObj(desired))
		desired.SetResourceVersion(existing.GetResourceVersion())
		return errors.Wrapf(ac.Update(ctx, desired, applyAct), "cannot update object")
	case v1alpha1.ResourceUpdateStrategyPatch:
		fallthrough
	default:
		klog.V(4).InfoS("patching object", "resource", klog.KObj(desired))
		patch, err := velapatch.ThreeWayMergePatch(existing, desired, &velapatch.PatchAction{
			AnnoLastAppliedConfig: oam.AnnotationLastAppliedConfig,
			AnnoLastAppliedTime:   oam.AnnotationLastAppliedTime,
			UpdateAnno:            applyAct.updateAnnotation,
		})
		if err != nil {
			return errors.Wrap(err, "cannot calculate patch by computing a three way diff")
		}
		if isEmptyPatch(patch) {
			return nil
		}
		return errors.Wrapf(ac.Patch(ctx, desired, patch, applyAct), "cannot patch object")
	}
}

func generateRenderHash(desired client.Object) (string, error) {
	if desired == nil {
		return "", nil
	}
	desiredHash, err := utils.ComputeSpecHash(desired)
	if err != nil {
		return "", errors.Wrap(err, "compute desired hash")
	}
	util.AddLabels(desired, map[string]string{
		LabelRenderHash: desiredHash,
	})
	return desiredHash, nil
}

// createOrGetExisting will create the object if it does not exist
// or get and return the existing object
func createOrGetExisting(ctx context.Context, act *applyAction, c client.Client, desired client.Object, ao ...ApplyOption) (client.Object, error) {
	var create = func() (client.Object, error) {
		// execute ApplyOptions even the object doesn't exist
		if err := executeApplyOptions(act, nil, desired, ao); err != nil {
			return nil, err
		}
		if act.readOnly {
			return nil, fmt.Errorf("%s (%s) is marked as read-only but does not exist. You should check the existence of the resource or remove the read-only policy", desired.GetObjectKind().GroupVersionKind().Kind, desired.GetName())
		}
		if act.updateAnnotation {
			if err := velapatch.AddLastAppliedConfiguration(desired, oam.AnnotationLastAppliedConfig, oam.AnnotationLastAppliedTime); err != nil {
				return nil, err
			}
		}
		klog.V(4).InfoS("creating object", "resource", klog.KObj(desired))
		return nil, errors.Wrap(c.Create(ctx, desired, act), "cannot create object")
	}

	if desired.GetObjectKind().GroupVersionKind().Kind == "" {
		gvk, err := apiutil.GVKForObject(desired, common.Scheme)
		if err == nil {
			desired.GetObjectKind().SetGroupVersionKind(gvk)
		}
	}

	// allow to create object with only generateName
	if desired.GetName() == "" && desired.GetGenerateName() != "" {
		return create()
	}

	existing := &unstructured.Unstructured{}
	existing.GetObjectKind().SetGroupVersionKind(desired.GetObjectKind().GroupVersionKind())
	err := c.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}, existing)
	if kerrors.IsNotFound(err) {
		return create()
	}
	if err != nil {
		return nil, errors.Wrap(err, "cannot get object")
	}
	return existing, nil
}

func executeApplyOptions(act *applyAction, existing, desired client.Object, aos []ApplyOption) error {
	// if existing is nil, it means the object is going to be created.
	// ApplyOption function should handle this situation carefully by itself.
	for _, fn := range aos {
		if err := fn(act, existing, desired); err != nil {
			return errors.Wrap(err, "cannot apply ApplyOption")
		}
	}
	return nil
}

// NotUpdateRenderHashEqual if the render hash of new object equal to the old hash, should not apply.
func NotUpdateRenderHashEqual() ApplyOption {
	return func(act *applyAction, existing, desired client.Object) error {
		if existing == nil || desired == nil || act.isShared {
			return nil
		}
		newSt, ok := desired.(*unstructured.Unstructured)
		if !ok {
			return nil
		}
		oldSt := existing.(*unstructured.Unstructured)
		if !ok {
			return nil
		}
		if k8s.GetLabel(existing, LabelRenderHash) == k8s.GetLabel(desired, LabelRenderHash) {
			*newSt = *oldSt
			act.skipUpdate = true
		}
		return nil
	}
}

// ReadOnly skip apply fo the resource
func ReadOnly() ApplyOption {
	return func(act *applyAction, _, _ client.Object) error {
		act.readOnly = true
		act.skipUpdate = true
		return nil
	}
}

// TakeOver allow take over resources without app owner
func TakeOver() ApplyOption {
	return func(act *applyAction, _, _ client.Object) error {
		act.takeOver = true
		return nil
	}
}

// WithUpdateStrategy set the update strategy for the apply operation
func WithUpdateStrategy(strategy v1alpha1.ResourceUpdateStrategy) ApplyOption {
	return func(act *applyAction, _, _ client.Object) error {
		act.updateStrategy = strategy
		return nil
	}
}

// MustBeControlledByApp requires that the new object is controllable by versioned resourcetracker
func MustBeControlledByApp(app *v1beta1.Application) ApplyOption {
	return func(act *applyAction, existing, _ client.Object) error {
		if existing == nil || act.isShared || act.readOnly {
			return nil
		}
		appKey, controlledBy := GetAppKey(app), GetControlledBy(existing)
		// if the existing object has no resource version, it means this resource is an API response not directly from
		// an etcd object but from some external services, such as vela-prism. Then the response does not necessarily
		// contain the owner
		if controlledBy == "" && !utilfeature.DefaultMutableFeatureGate.Enabled(features.LegacyResourceOwnerValidation) && existing.GetResourceVersion() != "" && !act.takeOver {
			return fmt.Errorf("%s %s/%s exists but not managed by any application now", existing.GetObjectKind().GroupVersionKind().Kind, existing.GetNamespace(), existing.GetName())
		}
		if controlledBy != "" && controlledBy != appKey {
			return fmt.Errorf("existing object %s %s/%s is managed by other application %s", existing.GetObjectKind().GroupVersionKind().Kind, existing.GetNamespace(), existing.GetName(), controlledBy)
		}
		return nil
	}
}

// GetControlledBy extract the application that controls the current resource
func GetControlledBy(existing client.Object) string {
	labels := existing.GetLabels()
	if labels == nil {
		return ""
	}
	appName := labels[oam.LabelAppName]
	appNs := labels[oam.LabelAppNamespace]
	if appName == "" || appNs == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s", appNs, appName)
}

// GetAppKey construct the key for identifying the application
func GetAppKey(app *v1beta1.Application) string {
	ns := app.Namespace
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	return fmt.Sprintf("%s/%s", ns, app.GetName())
}

// DisableUpdateAnnotation disable write last config to annotation
func DisableUpdateAnnotation() ApplyOption {
	return func(a *applyAction, existing, _ client.Object) error {
		a.updateAnnotation = false
		return nil
	}
}

// SharedByApp let the resource be sharable
func SharedByApp(app *v1beta1.Application) ApplyOption {
	return func(act *applyAction, existing, desired client.Object) error {
		// calculate the shared-by annotation
		// if resource exists, add the current application into the resource shared-by field
		var sharedBy string
		if existing != nil && existing.GetAnnotations() != nil {
			sharedBy = existing.GetAnnotations()[oam.AnnotationAppSharedBy]
		}
		sharedBy = AddSharer(sharedBy, app)
		util.AddAnnotations(desired, map[string]string{oam.AnnotationAppSharedBy: sharedBy})
		if existing == nil {
			return nil
		}

		// resource exists and controlled by current application
		appKey, controlledBy := GetAppKey(app), GetControlledBy(existing)
		if controlledBy == "" || appKey == controlledBy {
			return nil
		}

		// resource exists but not controlled by current application
		if existing.GetAnnotations() == nil || existing.GetAnnotations()[oam.AnnotationAppSharedBy] == "" {
			// if the application that controls the resource does not allow sharing, return error
			return fmt.Errorf("application is controlled by %s but is not sharable", controlledBy)
		}
		// the application that controls the resource allows sharing, then only mutate the shared-by annotation
		act.isShared = true
		bs, err := json.Marshal(existing)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(bs, desired); err != nil {
			return err
		}
		util.AddAnnotations(desired, map[string]string{oam.AnnotationAppSharedBy: sharedBy})
		return nil
	}
}

// DryRunAll executing all validation, etc without persisting the change to storage.
func DryRunAll() ApplyOption {
	return func(a *applyAction, existing, _ client.Object) error {
		a.dryRun = true
		return nil
	}
}

// isUpdatableResource check whether the resource is updatable
// Resource like v1.Service cannot unset the spec field (the ip spec is filled by service controller)
func isUpdatableResource(desired client.Object) bool {
	// nolint
	switch desired.GetObjectKind().GroupVersionKind() {
	case corev1.SchemeGroupVersion.WithKind("Service"):
		return false
	}
	return true
}

func isEmptyPatch(patch client.Patch) bool {
	if patch == nil {
		return true
	}
	data, _ := patch.Data(nil)
	return data != nil && string(data) == "{}"
}
