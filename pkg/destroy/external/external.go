// Package external removes the infrastructure of a cluster that was
// provisioned by a user-supplied Cluster API infrastructure provider.
package external

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1" //nolint:staticcheck //CORS-3563
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/installer/cmd/openshift-install/command"
	"github.com/openshift/installer/pkg/clusterapi"
	"github.com/openshift/installer/pkg/destroy/providers"
	"github.com/openshift/installer/pkg/infrastructure/external/hooks"
	"github.com/openshift/installer/pkg/types"
)

// deleteTimeout bounds the wait for the provider to remove the
// infrastructure. It is generous because it covers a whole cloud teardown --
// load balancers, NAT gateways and a VPC -- and because the failure it guards
// against is an unbounded hang, not slowness.
const deleteTimeout = 45 * time.Minute

// ClusterUninstaller holds the various options for the cluster we want to delete.
type ClusterUninstaller struct {
	Metadata *types.ClusterMetadata
	Logger   logrus.FieldLogger

	// Dir is the install directory, which holds both metadata.json and the
	// Cluster API artifacts the install left behind.
	Dir string
}

// New returns an External destroyer from ClusterMetadata.
func New(logger logrus.FieldLogger, metadata *types.ClusterMetadata) (providers.Destroyer, error) {
	return &ClusterUninstaller{
		Metadata: metadata,
		Logger:   logger,
		Dir:      command.RootOpts.Dir,
	}, nil
}

// Run removes the cluster's infrastructure by handing it back to the provider
// that created it.
//
// The installer has no SDK for the partner's cloud, so it cannot sweep
// resources by tag the way the integrated destroyers do. What it can do is
// rely on the Cluster API contract: deleting a Cluster deletes its
// infrastructure object, and the core controller does not release the
// Cluster's finalizer until that object is gone.
//
// The local control plane does not survive the install -- its etcd data
// directory is removed at the end of `create cluster` -- so the objects have
// to be recreated from the artifacts on disk. That makes this a restore
// followed by a delete, and the restore is modelled on what `clusterctl move`
// does between two management clusters: create everything paused, put the
// recorded state back, then unpause. The ordering is not incidental; see
// restore.
func (u *ClusterUninstaller) Run() (*types.ClusterQuota, error) {
	ctx := context.Background()

	artifactsDir := filepath.Join(u.Dir, clusterapi.ArtifactsDir)
	objects, err := loadArtifacts(artifactsDir)
	if err != nil {
		return nil, err
	}

	clusters := filter(objects, isCoreCluster)
	if len(clusters) == 0 {
		return nil, fmt.Errorf("no Cluster API Cluster found in %s: "+
			"this directory is written during provisioning and is what identifies the infrastructure to remove. "+
			"Without it the installer cannot tell the provider what to delete, and any infrastructure this cluster "+
			"created is still running and must be removed by hand", artifactsDir)
	}

	u.Logger.Infof("Restoring the local control plane to remove %d Cluster API cluster(s)", len(clusters))

	// Starting the system reads metadata.json and, for this platform, the
	// provider artifacts recorded there. A binary that has been moved or
	// replaced fails here, naming the path, before anything is deleted.
	if err := clusterapi.System().Run(ctx); err != nil {
		return nil, fmt.Errorf("failed to run the cluster api system: %w", err)
	}
	defer clusterapi.System().Teardown()
	defer clusterapi.System().CleanEtcd()

	cl := clusterapi.System().Client()

	restored, err := u.restore(ctx, cl, objects)
	if err != nil {
		return nil, err
	}
	if err := u.unpause(ctx, cl, restored); err != nil {
		return nil, err
	}

	// Before the delete, not after. Whatever the provisioning hook created is
	// invisible to Cluster API -- that is why it needed a hook -- so nothing
	// in the delete below will remove it. Running first also means those
	// resources can still be described in terms of the infrastructure they
	// refer to: a DNS alias record names the load balancer it points at, and
	// that load balancer is about to stop existing.
	//
	// A failure here aborts before anything is deleted, which leaves the
	// cluster whole and the destroy re-runnable.
	if err := u.runPreDestroyHook(ctx, restored, clusters); err != nil {
		return nil, err
	}

	for _, cluster := range clusters {
		if err := u.deleteCluster(ctx, cl, cluster); err != nil {
			return nil, err
		}
	}

	u.Logger.Info("Deleted the Cluster API cluster(s)")
	u.Logger.Warn("The provider reported the cluster deleted. Resources it did not create -- " +
		"for example a pre-existing network supplied to it -- are left in place by design, " +
		"so this is not a statement that nothing remains in the account.")

	return &types.ClusterQuota{}, nil
}

// runPreDestroyHook runs the teardown counterpart of the install's
// infra-ready hook.
//
// It is a no-op unless the install recorded one. That is the ordinary case:
// an install that created no out-of-band resources has none to remove, and an
// install from before hooks existed has no record of them either.
//
// The hook is given the same view of the cluster the provisioning hook had --
// the Cluster and the provider's infrastructure object, read back out of the
// restored control plane -- so that a single script can serve both directions
// without the two halves diverging over what they are told.
func (u *ClusterUninstaller) runPreDestroyHook(ctx context.Context, restored, clusters []client.Object) error {
	meta := u.Metadata.ClusterPlatformMetadata.External
	if meta == nil || meta.ClusterAPI == nil || meta.ClusterAPI.Hooks == nil || meta.ClusterAPI.Hooks.PreDestroy == nil {
		u.Logger.Debug("No pre-destroy hook recorded for this cluster")
		return nil
	}
	hook := meta.ClusterAPI.Hooks.PreDestroy

	req := hooks.Request{
		Kind:    hooks.PreDestroy,
		Program: hook.Program,
		Args:    hook.Args,
		// Recorded at install time because it differs by mode and there is
		// nothing here to work it out from: a hook from a provider payload
		// lives in the extracted payload, not in the External manifest
		// directory, and the install-config that would have said so is gone.
		ProgramRoot: meta.ClusterAPI.HookRoot,
		InstallDir:  u.Dir,
		InfraID:     u.Metadata.InfraID,
		ClusterName: u.Metadata.ClusterName,
		BaseDomain:  meta.BaseDomain,
	}

	if len(clusters) == 1 {
		if cluster, ok := clusters[0].(*clusterv1.Cluster); ok {
			req.ControlPlaneEndpointHost = cluster.Spec.ControlPlaneEndpoint.Host
			req.ControlPlaneEndpointPort = strconv.Itoa(int(cluster.Spec.ControlPlaneEndpoint.Port))
			if data, err := json.Marshal(cluster); err == nil {
				req.ClusterJSON = data
			}
			if infra := matchInfrastructure(restored, cluster); infra != nil {
				if data, err := json.Marshal(infra); err == nil {
					req.InfraJSON = data
				}
			}
		}
	}

	return hooks.Run(ctx, req)
}

// matchInfrastructure finds the object a Cluster's infrastructureRef points
// at, among the objects just restored. Matching on name and kind rather than
// on uid because the uids are new -- these objects were recreated a moment
// ago in a control plane that did not exist when the reference was written.
func matchInfrastructure(restored []client.Object, cluster *clusterv1.Cluster) client.Object {
	ref := cluster.Spec.InfrastructureRef
	if ref == nil {
		return nil
	}
	for _, obj := range restored {
		if obj.GetName() == ref.Name && kindOf(obj) == ref.Kind {
			return obj
		}
	}
	return nil
}

// restore recreates, paused, the objects the install applied, and returns
// them as they now exist in the control plane.
//
// Three properties of this function are load-bearing, and each guards against
// a failure that would otherwise be silent and expensive:
//
// Everything is created paused. The provider is already running by this
// point, so an unpaused object is reconciled the instant it appears --
// possibly before its status has been put back, in which case the provider
// sees a cluster it has no record of building and builds a second one.
// Pausing is how clusterctl move solves the same problem.
//
// Status is restored explicitly, because it is a subresource and Create drops
// it. Status is where the provider recorded the identifiers of what it built;
// without it, adoption has nothing to work from.
//
// Finalizers are restored rather than stripped. They were placed by the
// controllers that are about to run again, and a Cluster deleted before its
// controller has re-added its finalizer is removed immediately -- destroy
// then reports success while the whole cloud footprint is still running. The
// cost of restoring them is the opposite failure, a delete that hangs if the
// provider never reconciles, which is loud, bounded by deleteTimeout, and
// confined to a control plane that is thrown away regardless.
//
// Order within the restore matters too: the namespace first so the rest has
// somewhere to go, then the infrastructure objects, then the Clusters that
// reference them, so no Cluster is ever created with an infrastructureRef
// that does not resolve.
func (u *ClusterUninstaller) restore(ctx context.Context, cl client.Client, objects []client.Object) ([]client.Object, error) {
	phases := []struct {
		name  string
		match func(client.Object) bool
	}{
		{"namespace", isNamespace},
		{"infrastructure", func(o client.Object) bool { return !isNamespace(o) && !isCoreCluster(o) }},
		{"cluster", isCoreCluster},
	}

	var restored []client.Object
	for _, phase := range phases {
		for _, obj := range filter(objects, phase.match) {
			live, err := u.create(ctx, cl, obj)
			if err != nil {
				return nil, fmt.Errorf("failed to restore %s %s %s/%s: %w",
					phase.name, kindOf(obj), obj.GetNamespace(), obj.GetName(), err)
			}
			restored = append(restored, live)
		}
	}
	return restored, nil
}

// create applies one object, paused, then writes its recorded status back.
func (u *ClusterUninstaller) create(ctx context.Context, cl client.Client, obj client.Object) (client.Object, error) {
	live := copyObject(obj)

	// resourceVersion and uid identify the object in a control plane that no
	// longer exists; the API server rejects a Create that carries them.
	// Owner references are dropped for the same reason -- they point at uids
	// that will not exist here -- and the controllers re-establish them.
	live.SetResourceVersion("")
	live.SetUID("")
	live.SetManagedFields(nil)
	live.SetOwnerReferences(nil)
	pause(live)

	switch err := cl.Create(ctx, live); {
	case apierrors.IsAlreadyExists(err):
		// A re-run of destroy after an interrupted one. Take what is there.
		if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			return nil, fmt.Errorf("failed to read existing object: %w", err)
		}
		u.Logger.Debugf("%s %s/%s already present", kindOf(obj), obj.GetNamespace(), obj.GetName())
		return live, nil
	case err != nil:
		return nil, err
	}
	u.Logger.Infof("Restored %s %s/%s", kindOf(obj), obj.GetNamespace(), obj.GetName())

	// A Namespace's status is its phase, which the API server owns.
	if isNamespace(obj) {
		return live, nil
	}

	status := copyObject(obj)
	status.SetResourceVersion(live.GetResourceVersion())
	status.SetUID(live.GetUID())
	status.SetManagedFields(nil)
	status.SetOwnerReferences(nil)
	pause(status)
	if err := cl.Status().Update(ctx, status); err != nil {
		// Not fatal: a kind with no status subresource has nothing to carry
		// over, and that is indistinguishable here from a kind that does.
		// Warn rather than debug -- if the provider does go on to build a
		// second copy of the infrastructure, this line is the explanation.
		u.Logger.Warnf("Could not restore the recorded status of %s %s/%s, so the provider will have to "+
			"rediscover what it built: %v", kindOf(obj), obj.GetNamespace(), obj.GetName(), err)
		return live, nil
	}
	live.SetResourceVersion(status.GetResourceVersion())
	return live, nil
}

// unpause hands the restored objects to the provider.
//
// It runs only after every object and status is in place, so the first
// reconcile of each object sees the complete recorded state rather than a
// partial one.
func (u *ClusterUninstaller) unpause(ctx context.Context, cl client.Client, objects []client.Object) error {
	u.Logger.Info("Handing the restored objects back to the provider")
	for _, obj := range objects {
		if isNamespace(obj) {
			continue
		}
		base := copyObject(obj)
		unpause(obj)
		if err := cl.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("failed to unpause %s %s/%s: %w",
				kindOf(obj), obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

// deleteCluster deletes one Cluster and waits for it to disappear.
//
// Waiting is the whole point. The core Cluster controller deletes the
// infrastructure object and does not clear the Cluster's own finalizer until
// that object is gone, so the Cluster outliving the delete request is the
// signal that the provider has not finished, and the Cluster disappearing is
// the provider's own report that it has.
func (u *ClusterUninstaller) deleteCluster(ctx context.Context, cl client.Client, cluster client.Object) error {
	key := client.ObjectKeyFromObject(cluster)

	current := &clusterv1.Cluster{}
	if err := cl.Get(ctx, key, current); err != nil {
		return fmt.Errorf("failed to read Cluster %s/%s before deleting it: %w", key.Namespace, key.Name, err)
	}
	if len(current.GetFinalizers()) == 0 {
		// Nothing would hold the object open, so the delete would return
		// immediately and this function would report success having removed
		// nothing. Refuse rather than mislead.
		return fmt.Errorf("cluster %s/%s has no finalizer, so deleting it would remove the record of the "+
			"infrastructure without removing the infrastructure. The Cluster API artifacts in %s were written "+
			"before the provider took ownership of this cluster, or were edited afterwards. The infrastructure "+
			"is still running and must be removed through the provider or by hand",
			key.Namespace, key.Name, filepath.Join(u.Dir, clusterapi.ArtifactsDir))
	}

	u.Logger.Infof("Deleting Cluster %s/%s", key.Namespace, key.Name)
	if err := cl.Delete(ctx, current); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete Cluster %s/%s: %w", key.Namespace, key.Name, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		observed := &clusterv1.Cluster{}
		switch err := cl.Get(ctx, key, observed); {
		case apierrors.IsNotFound(err):
			return true, nil
		case err != nil:
			// Transient while the control plane settles. Keep polling rather
			// than abandoning a delete that is already under way.
			u.Logger.Debugf("Waiting for Cluster %s/%s: %v", key.Namespace, key.Name, err)
			return false, nil
		}
		u.Logger.Debugf("Cluster %s/%s still present, phase %q", key.Namespace, key.Name, observed.Status.Phase)
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("timed out after %s waiting for Cluster %s/%s to be deleted: the provider did not "+
			"finish removing the infrastructure, which is still running. Re-running destroy is safe and will "+
			"resume: %w", deleteTimeout, key.Namespace, key.Name, err)
	}
	return nil
}

// loadArtifacts reads the manifests the install collected.
//
// Files are read in sorted order so a run is reproducible, and every document
// is decoded through the same path provisioning used, so a kind this
// installer has no Go type for is carried through unresolved rather than
// rejected -- the ordinary case for a user-supplied provider.
func loadArtifacts(dir string) ([]client.Object, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("failed to list %s: %w", dir, err)
	}
	if len(entries) == 0 {
		if _, statErr := os.Stat(dir); errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("%s does not exist: the Cluster API artifacts this cluster was provisioned "+
				"with are not in this install directory, so there is nothing to identify the infrastructure "+
				"to remove", dir)
		}
		return nil, fmt.Errorf("no manifests found in %s", dir)
	}
	sort.Strings(entries)

	var objects []client.Object
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", path, err)
		}
		decoded, err := clusterapi.ObjectsFromManifest(path, data)
		if err != nil {
			return nil, fmt.Errorf("failed to decode %s: %w", path, err)
		}
		for _, d := range decoded {
			objects = append(objects, d.Object)
		}
	}
	return objects, nil
}

// pause marks an object as not to be reconciled.
//
// Both mechanisms are used because they are not interchangeable: the core
// Cluster controller reads spec.paused, and every other controller reads the
// annotation, which is also what propagates to objects the Cluster owns.
func pause(obj client.Object) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[clusterv1.PausedAnnotation] = "true"
	obj.SetAnnotations(annotations)

	if cluster, ok := obj.(*clusterv1.Cluster); ok {
		cluster.Spec.Paused = true
	}
}

// unpause reverses pause.
func unpause(obj client.Object) {
	annotations := obj.GetAnnotations()
	delete(annotations, clusterv1.PausedAnnotation)
	obj.SetAnnotations(annotations)

	if cluster, ok := obj.(*clusterv1.Cluster); ok {
		cluster.Spec.Paused = false
	}
}

// copyObject deep-copies a client.Object as a client.Object.
func copyObject(obj client.Object) client.Object {
	//nolint:forcetypeassert // The deep copy of a client.Object is a client.Object.
	return obj.DeepCopyObject().(client.Object)
}

// filter returns the objects matching keep, preserving order.
func filter(objects []client.Object, keep func(client.Object) bool) []client.Object {
	var out []client.Object
	for _, obj := range objects {
		if keep(obj) {
			out = append(out, obj)
		}
	}
	return out
}

// isCoreCluster reports whether obj is a core Cluster API Cluster.
//
// The check is on the Go type, matching how provisioning finds the same
// object. A Cluster at a core API version this installer was not built
// against decodes as unstructured and is deliberately not matched: the
// destroyer would have no typed access to its status or finalizers, so
// treating it as a Cluster would mean deleting it without being able to tell
// whether the delete took effect.
func isCoreCluster(obj client.Object) bool {
	_, ok := obj.(*clusterv1.Cluster)
	return ok
}

// isNamespace reports whether obj is a core Namespace.
func isNamespace(obj client.Object) bool {
	return kindOf(obj) == "Namespace"
}

// kindOf names an object's kind.
//
// The GVK carried on the object is checked first, which is what an
// unstructured object has and a typed one generally does not: decoding
// through the scheme leaves TypeMeta empty, so a typed object has to be
// identified by looking its Go type up in the scheme instead.
func kindOf(obj client.Object) string {
	if kind := obj.GetObjectKind().GroupVersionKind().Kind; kind != "" {
		return kind
	}
	if gvks, _, err := clusterapi.Scheme.ObjectKinds(obj); err == nil && len(gvks) > 0 {
		return preferredKind(gvks)
	}
	return fmt.Sprintf("%T", obj)
}

// preferredKind picks one kind from an ambiguous scheme lookup. Every
// candidate is the same Go type, so the kinds agree; only the group or
// version can differ, and none of the callers here distinguish them.
func preferredKind(gvks []schema.GroupVersionKind) string {
	return gvks[0].Kind
}
