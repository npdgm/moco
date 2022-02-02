package controllers

import (
	"context"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"

	mocov1beta2 "github.com/cybozu-go/moco/api/v1beta2"
	"github.com/cybozu-go/moco/clustering"
	"github.com/cybozu-go/moco/pkg/constants"
	"github.com/cybozu-go/moco/pkg/mycnf"
	"github.com/cybozu-go/moco/pkg/password"
	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	defaultTerminationGracePeriodSeconds = 300
	fieldManager                         = "moco-controller"
)

// debug and test variables
var (
	debugController = os.Getenv("DEBUG_CONTROLLER") == "1"
	noJobResource   = os.Getenv("TEST_NO_JOB_RESOURCE") == "1"
)

// `controller` should be true only if the resource is created in the same namespace as moco-controller.
func labelSet(cluster *mocov1beta2.MySQLCluster, controller bool) map[string]string {
	labels := map[string]string{
		constants.LabelAppName:      constants.AppNameMySQL,
		constants.LabelAppInstance:  cluster.Name,
		constants.LabelAppCreatedBy: constants.AppCreator,
	}
	if controller {
		labels[constants.LabelAppNamespace] = cluster.Namespace
	}
	return labels
}

func labelSetForJob(cluster *mocov1beta2.MySQLCluster) map[string]string {
	labels := map[string]string{
		constants.LabelAppName:      constants.AppNameBackup,
		constants.LabelAppInstance:  cluster.Name,
		constants.LabelAppCreatedBy: constants.AppCreator,
	}
	return labels
}

func mergeMap(m1, m2 map[string]string) map[string]string {
	m := make(map[string]string)
	for k, v := range m1 {
		m[k] = v
	}
	for k, v := range m2 {
		m[k] = v
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// MySQLClusterReconciler reconciles a MySQLCluster object
type MySQLClusterReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	AgentImage      string
	BackupImage     string
	FluentBitImage  string
	ExporterImage   string
	SystemNamespace string
	ClusterManager  clustering.ClusterManager
}

//+kubebuilder:rbac:groups=moco.cybozu.com,resources=mysqlclusters,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=moco.cybozu.com,resources=mysqlclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=moco.cybozu.com,resources=mysqlclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=moco.cybozu.com,resources=backuppolicies,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=statefulsets/status,verbs=get
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets/status,verbs=get
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services/status,verbs=get
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts/status,verbs=get
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get
//+kubebuilder:rbac:groups="",resources=events,verbs=create;update;patch
//+kubebuilder:rbac:groups="policy",resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="cert-manager.io",resources=certificates,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups="batch",resources=cronjobs;jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements Reconciler interface.
// See https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile#Reconciler
func (r *MySQLClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := crlog.FromContext(ctx)

	cluster := &mocov1beta2.MySQLCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			r.ClusterManager.Stop(req.NamespacedName)
			return ctrl.Result{}, nil
		}

		log.Error(err, "unable to fetch MySQLCluster")
		return ctrl.Result{}, err
	}

	// The highest reconciler version
	reconciler := r.reconcileV1

	// A MySQLCluster reconciler should create or update Kubernetes resources
	// in a consistent manner until the MySQLCluster resource is updated
	// so that MySQL would not get restarted when MOCO is updated.
	// Therefore, we implement multiple reconcilers and gives different
	// versions to them.
	if cluster.Status.ReconcileInfo.Generation == cluster.Generation || cluster.DeletionTimestamp != nil {
		switch cluster.Status.ReconcileInfo.ReconcileVersion {
		case 0:
			// prefer the highest version
		case 1:
			reconciler = r.reconcileV1
		}
	}
	return reconciler(ctx, req, cluster)
}

func (r *MySQLClusterReconciler) reconcileV1(ctx context.Context, req ctrl.Request, cluster *mocov1beta2.MySQLCluster) (ctrl.Result, error) {
	log := crlog.FromContext(ctx)

	if cluster.DeletionTimestamp != nil {
		if !controllerutil.ContainsFinalizer(cluster, constants.MySQLClusterFinalizer) {
			return ctrl.Result{}, nil
		}

		log.Info("start finalizing MySQLCluster")

		r.ClusterManager.Stop(req.NamespacedName)

		if err := r.finalizeV1(ctx, cluster); err != nil {
			log.Error(err, "failed to finalize")
			return ctrl.Result{}, err
		}

		controllerutil.RemoveFinalizer(cluster, constants.MySQLClusterFinalizer)
		if err := r.Update(ctx, cluster); err != nil {
			log.Error(err, "failed to remove finalizer")
			return ctrl.Result{}, err
		}

		log.Info("finalizing MySQLCluster is completed")

		return ctrl.Result{}, nil
	}

	if err := r.reconcileV1Secret(ctx, req, cluster); err != nil {
		log.Error(err, "failed to reconcile secret")
		return ctrl.Result{}, err
	}

	if err := r.reconcileV1Certificate(ctx, req, cluster); err != nil {
		log.Error(err, "failed to reconcile certificate")
		return ctrl.Result{}, err
	}

	if err := r.reconcileV1GRPCSecret(ctx, req, cluster); err != nil {
		log.Error(err, "failed to reconcile gRPC secret")
		return ctrl.Result{}, err
	}

	mycnf, err := r.reconcileV1MyCnf(ctx, req, cluster)
	if err != nil {
		log.Error(err, "failed to reconcile my.conf config map")
		return ctrl.Result{}, err
	}

	if err := r.reconcileV1FluentBitConfigMap(ctx, req, cluster); err != nil {
		log.Error(err, "failed to reconcile config maps for fluent-bit")
		return ctrl.Result{}, err
	}

	if err := r.reconcileV1ServiceAccount(ctx, req, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileV1Service(ctx, req, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileV1StatefulSet(ctx, req, cluster, mycnf); err != nil {
		log.Error(err, "failed to reconcile stateful set")
		return ctrl.Result{}, err
	}

	if err := r.reconcileV1PDB(ctx, req, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileV1BackupJob(ctx, req, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileV1RestoreJob(ctx, req, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if cluster.Status.ReconcileInfo.Generation != cluster.Generation {
		cluster.Status.ReconcileInfo.Generation = cluster.Generation
		cluster.Status.ReconcileInfo.ReconcileVersion = 1
		if err := r.Status().Update(ctx, cluster); err != nil {
			log.Error(err, "failed to update reconciliation info")
			return ctrl.Result{}, err
		}
	}

	r.ClusterManager.Update(client.ObjectKeyFromObject(cluster))
	return ctrl.Result{}, nil
}

func (r *MySQLClusterReconciler) reconcileV1Secret(ctx context.Context, req ctrl.Request, cluster *mocov1beta2.MySQLCluster) error {
	log := crlog.FromContext(ctx)

	secretName := cluster.ControllerSecretName()
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: r.SystemNamespace, Name: secretName}, secret)
	if err == nil {
		passwd, err := password.NewMySQLPasswordFromSecret(secret)
		if err != nil {
			return fmt.Errorf("failed to create password from secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
		userSecret := &corev1.Secret{}
		userSecret.Namespace = cluster.Namespace
		userSecret.Name = cluster.UserSecretName()
		result, err := ctrl.CreateOrUpdate(ctx, r.Client, userSecret, func() error {
			newSecret := passwd.ToSecret()
			userSecret.Annotations = mergeMap(userSecret.Annotations, newSecret.Annotations)
			userSecret.Labels = mergeMap(userSecret.Labels, labelSet(cluster, false))
			userSecret.Data = newSecret.Data
			return ctrl.SetControllerReference(cluster, userSecret, r.Scheme)
		})
		if err != nil {
			return err
		}
		if result != controllerutil.OperationResultNone {
			log.Info("reconciled user secret", "operation", string(result))
		}

		mycnfSecret := &corev1.Secret{}
		mycnfSecret.Namespace = cluster.Namespace
		mycnfSecret.Name = cluster.MyCnfSecretName()
		result, err = ctrl.CreateOrUpdate(ctx, r.Client, mycnfSecret, func() error {
			newSecret := passwd.ToMyCnfSecret()
			mycnfSecret.Annotations = mergeMap(mycnfSecret.Annotations, newSecret.Annotations)
			mycnfSecret.Labels = mergeMap(mycnfSecret.Labels, labelSet(cluster, false))
			mycnfSecret.Data = newSecret.Data
			return ctrl.SetControllerReference(cluster, mycnfSecret, r.Scheme)
		})
		if err != nil {
			return err
		}
		if result != controllerutil.OperationResultNone {
			log.Info("reconciled my.cnf secret", "operation", string(result))
		}

		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	passwd, err := password.NewMySQLPassword()
	if err != nil {
		return err
	}

	secret = passwd.ToSecret()
	secret.Namespace = r.SystemNamespace
	secret.Name = secretName
	secret.Labels = labelSet(cluster, true)
	if err := r.Client.Create(ctx, secret); err != nil {
		return err
	}

	userSecret := passwd.ToSecret()
	userSecret.Namespace = cluster.Namespace
	userSecret.Name = cluster.UserSecretName()
	userSecret.Labels = labelSet(cluster, false)
	if err := ctrl.SetControllerReference(cluster, userSecret, r.Scheme); err != nil {
		return err
	}
	if err := r.Client.Create(ctx, userSecret); err != nil {
		return err
	}

	mycnfSecret := passwd.ToMyCnfSecret()
	mycnfSecret.Namespace = cluster.Namespace
	mycnfSecret.Name = cluster.MyCnfSecretName()
	mycnfSecret.Labels = labelSet(cluster, false)
	if err := ctrl.SetControllerReference(cluster, mycnfSecret, r.Scheme); err != nil {
		return err
	}
	if err := r.Client.Create(ctx, mycnfSecret); err != nil {
		return err
	}

	return nil
}

func (r *MySQLClusterReconciler) reconcileV1MyCnf(ctx context.Context, req ctrl.Request, cluster *mocov1beta2.MySQLCluster) (*corev1.ConfigMap, error) {
	log := crlog.FromContext(ctx)

	var mysqldContainer *corev1ac.ContainerApplyConfiguration
	for i, c := range cluster.Spec.PodTemplate.Spec.Containers {
		if *c.Name == constants.MysqldContainerName {
			mysqldContainer = &cluster.Spec.PodTemplate.Spec.Containers[i]
			break
		}
	}
	if mysqldContainer == nil {
		return nil, fmt.Errorf("MySQLD container not found")
	}

	// resources.requests.memory takes precedence over resources.limits.memory.
	var totalMem int64
	if mysqldContainer.Resources != nil {
		if mysqldContainer.Resources.Limits != nil {
			if res := mysqldContainer.Resources.Limits.Memory(); !res.IsZero() {
				totalMem = res.Value()
			}
		}

		if mysqldContainer.Resources.Requests != nil {
			if res := mysqldContainer.Resources.Requests.Memory(); !res.IsZero() {
				totalMem = res.Value()
			}
		}
	}

	var userConf map[string]string
	if cluster.Spec.MySQLConfigMapName != nil {
		cm := &corev1.ConfigMap{}
		err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: *cluster.Spec.MySQLConfigMapName}, cm)
		if err != nil {
			log.Error(err, "failed to get specified configmap", "configmap", *cluster.Spec.MySQLConfigMapName)
			return nil, err
		}
		userConf = cm.Data
	}

	conf := mycnf.Generate(userConf, totalMem)

	fnv32a := fnv.New32a()
	fnv32a.Write([]byte(conf))
	suffix := hex.EncodeToString(fnv32a.Sum(nil))

	prefix := cluster.PrefixedName() + "."

	cm := &corev1.ConfigMap{}
	cm.Namespace = cluster.Namespace
	cm.Name = prefix + suffix
	result, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = mergeMap(cm.Labels, labelSet(cluster, false))
		cm.Data = map[string]string{
			constants.MySQLConfName: conf,
		}
		return ctrl.SetControllerReference(cluster, cm, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	if result != controllerutil.OperationResultNone {
		log.Info("reconciled my.cnf configmap", "operation", string(result))
	}

	cms := &corev1.ConfigMapList{}
	if err := r.List(ctx, cms, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}
	for _, old := range cms.Items {
		if strings.HasPrefix(old.Name, prefix) && old.Name != cm.Name {
			if err := r.Delete(ctx, &old); err != nil {
				return nil, fmt.Errorf("failed to delete old my.cnf configmap %s/%s: %w", old.Namespace, old.Name, err)
			}
		}
	}

	return cm, nil
}

func (r *MySQLClusterReconciler) reconcileV1FluentBitConfigMap(ctx context.Context, req ctrl.Request, cluster *mocov1beta2.MySQLCluster) error {
	log := crlog.FromContext(ctx)

	configTmpl := `[SERVICE]
  Log_Level      error
[INPUT]
  Name           tail
  Path           %s
  Read_from_Head true
[OUTPUT]
  Name           file
  Match          *
  Path           /dev
  File           stdout
  Format         template
  Template       {log}
`

	if !cluster.Spec.DisableSlowQueryLogContainer {
		cm := &corev1.ConfigMap{}
		cm.Namespace = cluster.Namespace
		cm.Name = cluster.SlowQueryLogAgentConfigMapName()
		result, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
			cm.Labels = mergeMap(cm.Labels, labelSet(cluster, false))
			confVal := fmt.Sprintf(configTmpl, filepath.Join(constants.LogDirPath, constants.MySQLSlowLogName))
			cm.Data = map[string]string{
				constants.FluentBitConfigName: confVal,
			}
			return ctrl.SetControllerReference(cluster, cm, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("failed to reconcile configmap for slow logs: %w", err)
		}
		if result != controllerutil.OperationResultNone {
			log.Info("reconciled configmap for slow logs", "operation", string(result))
		}
	} else {
		cm := &corev1.ConfigMap{}
		cm.Namespace = cluster.Namespace
		cm.Name = cluster.SlowQueryLogAgentConfigMapName()
		err := r.Client.Delete(ctx, cm)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete configmap for slow logs: %w", err)
		}
	}

	return nil
}

func (r *MySQLClusterReconciler) reconcileV1ServiceAccount(ctx context.Context, req ctrl.Request, cluster *mocov1beta2.MySQLCluster) error {
	log := crlog.FromContext(ctx)

	sa := &corev1.ServiceAccount{}
	sa.Namespace = cluster.Namespace
	sa.Name = cluster.PrefixedName()

	result, err := ctrl.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = mergeMap(sa.Labels, labelSet(cluster, false))
		return ctrl.SetControllerReference(cluster, sa, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile service account: %w", err)
	}
	if result != controllerutil.OperationResultNone {
		log.Info("reconciled service account", "operation", string(result))
	}

	return nil
}

func (r *MySQLClusterReconciler) reconcileV1Service(ctx context.Context, req ctrl.Request, cluster *mocov1beta2.MySQLCluster) error {
	if err := r.reconcileV1Service1(ctx, cluster, nil, cluster.HeadlessServiceName(), true, labelSet(cluster, false)); err != nil {
		return err
	}

	primarySelector := labelSet(cluster, false)
	primarySelector[constants.LabelMocoRole] = constants.RolePrimary
	if err := r.reconcileV1Service1(ctx, cluster, cluster.Spec.PrimaryServiceTemplate, cluster.PrimaryServiceName(), false, primarySelector); err != nil {
		return err
	}

	replicaSelector := labelSet(cluster, false)
	replicaSelector[constants.LabelMocoRole] = constants.RoleReplica
	if err := r.reconcileV1Service1(ctx, cluster, cluster.Spec.ReplicaServiceTemplate, cluster.ReplicaServiceName(), false, replicaSelector); err != nil {
		return err
	}
	return nil
}

func (r *MySQLClusterReconciler) reconcileV1Service1(ctx context.Context, cluster *mocov1beta2.MySQLCluster, template *mocov1beta2.ServiceTemplate, name string, headless bool, selector map[string]string) error {
	log := crlog.FromContext(ctx)

	svc := corev1ac.Service(name, cluster.Namespace).WithSpec(corev1ac.ServiceSpec())

	tmpl := template.DeepCopy()

	if !headless && tmpl != nil {
		svc.WithAnnotations(tmpl.Annotations).
			WithLabels(tmpl.Labels).
			WithLabels(labelSet(cluster, false))

		if tmpl.Spec != nil {
			s := (*corev1ac.ServiceSpecApplyConfiguration)(tmpl.Spec)
			svc.WithSpec(s)
		}
	} else {
		svc.WithLabels(labelSet(cluster, false))
	}

	if headless {
		svc.Spec.WithClusterIP(corev1.ClusterIPNone).
			WithType(corev1.ServiceTypeClusterIP).
			WithPublishNotReadyAddresses(true)
	}

	svc.Spec.WithSelector(selector)

	var mysqlNodePort, mysqlXNodePort int32
	for _, p := range svc.Spec.Ports {
		switch *p.Name {
		case constants.MySQLPortName:
			mysqlNodePort = *p.NodePort
		case constants.MySQLXPortName:
			mysqlXNodePort = *p.NodePort
		}
	}

	svc.Spec.WithPorts(
		corev1ac.ServicePort().
			WithName(constants.MySQLPortName).
			WithProtocol(corev1.ProtocolTCP).
			WithPort(constants.MySQLPort).
			WithTargetPort(intstr.FromString(constants.MySQLPortName)).
			WithNodePort(mysqlNodePort),
		corev1ac.ServicePort().
			WithName(constants.MySQLXPortName).
			WithProtocol(corev1.ProtocolTCP).
			WithPort(constants.MySQLXPort).
			WithTargetPort(intstr.FromString(constants.MySQLXPortName)).
			WithNodePort(mysqlXNodePort),
	)

	if err := setControllerReferenceWithService(cluster, svc, r.Scheme); err != nil {
		return fmt.Errorf("failed to set ownerReference to Service %s/%s: %w", cluster.Namespace, name, err)
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(svc)
	if err != nil {
		return fmt.Errorf("failed to convert Service %s/%s to unstructured: %w", cluster.Namespace, name, err)
	}
	patch := &unstructured.Unstructured{
		Object: obj,
	}

	var orig, updated corev1.Service
	err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, &orig)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get Service %s/%s: %w", cluster.Namespace, name, err)
	}

	origApplyConfig, err := corev1ac.ExtractService(&orig, fieldManager)
	if err != nil {
		return fmt.Errorf("failed to extract Service %s/%s: %w", cluster.Namespace, name, err)
	}

	if equality.Semantic.DeepEqual(svc, origApplyConfig) {
		return nil
	}

	err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
		FieldManager: fieldManager,
		Force:        pointer.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile %s service: %w", name, err)
	}

	if debugController {
		if err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, &updated); err != nil {
			return fmt.Errorf("failed to get Service %s/%s: %w", cluster.Namespace, name, err)
		}

		if diff := cmp.Diff(orig, updated); len(diff) > 0 {
			fmt.Println(diff)
		}
	}

	log.Info("reconciled service", "name", name)

	return nil
}

func (r *MySQLClusterReconciler) reconcileV1StatefulSet(ctx context.Context, req ctrl.Request, cluster *mocov1beta2.MySQLCluster, mycnf *corev1.ConfigMap) error {
	log := crlog.FromContext(ctx)

	var orig appsv1.StatefulSet
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.PrefixedName()}, &orig)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get StatefulSet %s/%s: %w", cluster.Namespace, cluster.PrefixedName(), err)
	}

	sts := appsv1ac.StatefulSet(cluster.PrefixedName(), cluster.Namespace).
		WithLabels(labelSet(cluster, false)).
		WithSpec(appsv1ac.StatefulSetSpec().
			WithReplicas(cluster.Spec.Replicas).
			WithSelector(metav1ac.LabelSelector().
				WithMatchLabels(labelSet(cluster, false))).
			WithPodManagementPolicy(appsv1.ParallelPodManagement).
			WithUpdateStrategy(appsv1ac.StatefulSetUpdateStrategy().
				WithType(appsv1.RollingUpdateStatefulSetStrategyType)).
			WithServiceName(cluster.HeadlessServiceName()))

	volumeClaimTemplates := make([]*corev1ac.PersistentVolumeClaimApplyConfiguration, 0, len(cluster.Spec.VolumeClaimTemplates))
	for _, v := range cluster.Spec.VolumeClaimTemplates {
		pvc := v.ToCoreV1()

		if err := setControllerReferenceWithPVC(cluster, pvc, r.Scheme); err != nil {
			return fmt.Errorf("failed to set ownerReference to PVC %s/%s: %w", cluster.Namespace, *pvc.Name, err)
		}

		volumeClaimTemplates = append(volumeClaimTemplates, pvc)
	}
	sts.Spec.WithVolumeClaimTemplates(volumeClaimTemplates...)

	sts.Spec.WithTemplate(corev1ac.PodTemplateSpec().
		WithAnnotations(cluster.Spec.PodTemplate.Annotations).
		WithLabels(cluster.Spec.PodTemplate.Labels).
		WithLabels(labelSet(cluster, false)))

	podSpec := corev1ac.PodSpecApplyConfiguration(*cluster.Spec.PodTemplate.Spec.DeepCopy())
	podSpec.WithServiceAccountName(cluster.PrefixedName())

	if podSpec.TerminationGracePeriodSeconds == nil {
		podSpec.WithTerminationGracePeriodSeconds(defaultTerminationGracePeriodSeconds)
	}

	podSpec.WithVolumes(
		corev1ac.Volume().
			WithName(constants.TmpVolumeName).
			// If you use this, the EmptyDir will not be nil and will not match for "equality.Semantic.DeepEqual".
			// WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
			WithEmptyDir(nil),
		corev1ac.Volume().
			WithName(constants.RunVolumeName).
			WithEmptyDir(nil),
		corev1ac.Volume().
			WithName(constants.VarLogVolumeName).
			WithEmptyDir(nil),
		corev1ac.Volume().
			WithName(constants.MySQLInitConfVolumeName).
			WithEmptyDir(nil),
		corev1ac.Volume().
			WithName(constants.MySQLConfVolumeName).
			WithConfigMap(corev1ac.ConfigMapVolumeSource().
				WithName(mycnf.Name).WithDefaultMode(0644)),
		corev1ac.Volume().
			WithName(constants.MySQLConfSecretVolumeName).
			WithSecret(corev1ac.SecretVolumeSource().
				WithSecretName(cluster.MyCnfSecretName()).
				WithDefaultMode(0644)),
		corev1ac.Volume().
			WithName(constants.GRPCSecretVolumeName).
			WithSecret(corev1ac.SecretVolumeSource().
				WithSecretName(cluster.GRPCSecretName()).
				WithDefaultMode(0644)),
	)

	if !cluster.Spec.DisableSlowQueryLogContainer {
		podSpec.WithVolumes(
			corev1ac.Volume().
				WithName(constants.SlowQueryLogAgentConfigVolumeName).
				WithConfigMap(corev1ac.ConfigMapVolumeSource().
					WithName(cluster.SlowQueryLogAgentConfigMapName()).
					WithDefaultMode(0644)),
		)
	}

	containers := make([]*corev1ac.ContainerApplyConfiguration, 0, 4)

	mysqldContainer, err := r.makeV1MySQLDContainer(cluster)
	if err != nil {
		return err
	}
	containers = append(containers, mysqldContainer)
	containers = append(containers, r.makeV1AgentContainer(cluster))

	if !cluster.Spec.DisableSlowQueryLogContainer {
		force := cluster.Status.ReconcileInfo.Generation != cluster.Generation
		sts, err := appsv1ac.ExtractStatefulSet(&orig, fieldManager)
		if err != nil {
			return fmt.Errorf("failed to extract StatefulSet: %w", err)
		}

		containers = append(containers, r.makeV1SlowQueryLogContainer(sts, force))
	}
	if len(cluster.Spec.Collectors) > 0 {
		containers = append(containers, r.makeV1ExporterContainer(cluster.Spec.Collectors))
	}
	containers = append(containers, r.makeV1OptionalContainers(cluster)...)

	initContainers := r.makeV1InitContainer(cluster, *mysqldContainer.Image)

	podSpec.Containers = nil
	podSpec.InitContainers = nil
	podSpec.WithContainers(containers...)
	podSpec.WithInitContainers(initContainers...)

	sts.Spec.Template.WithSpec(&podSpec)

	if err := setControllerReferenceWithStatefulSet(cluster, sts, r.Scheme); err != nil {
		return fmt.Errorf("failed to set ownerReference to StatefulSet %s/%s: %w", cluster.Namespace, cluster.PrefixedName(), err)
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(sts)
	if err != nil {
		return fmt.Errorf("failed to convert StatefulSet %s/%s to unstructured: %w", cluster.Namespace, cluster.PrefixedName(), err)
	}
	patch := &unstructured.Unstructured{
		Object: obj,
	}

	origApplyConfig, err := appsv1ac.ExtractStatefulSet(&orig, fieldManager)
	if err != nil {
		return fmt.Errorf("failed to extract StatefulSet %s/%s: %w", cluster.Namespace, cluster.PrefixedName(), err)
	}

	if equality.Semantic.DeepEqual(sts, origApplyConfig) {
		return nil
	}

	err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
		FieldManager: fieldManager,
		Force:        pointer.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile stateful set: %w", err)
	}

	if debugController {
		var updated appsv1.StatefulSet
		if err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.PrefixedName()}, &updated); err != nil {
			return fmt.Errorf("failed to get StatefulSet %s/%s: %w", cluster.Namespace, cluster.PrefixedName(), err)
		}

		if diff := cmp.Diff(orig, updated); len(diff) > 0 {
			fmt.Println(diff)
		}
	}

	log.Info("reconciled stateful set", "name", cluster.PrefixedName())

	return nil
}

func (r *MySQLClusterReconciler) reconcileV1PDB(ctx context.Context, req ctrl.Request, cluster *mocov1beta2.MySQLCluster) error {
	log := crlog.FromContext(ctx)

	pdb := &policyv1beta1.PodDisruptionBudget{}
	pdb.Namespace = cluster.Namespace
	pdb.Name = cluster.PrefixedName()
	if cluster.Spec.Replicas < 3 {
		err := r.Delete(ctx, pdb)
		if err == nil {
			log.Info("removed pod disruption budget")
		}
		return client.IgnoreNotFound(err)
	}

	result, err := ctrl.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Labels = mergeMap(pdb.Labels, labelSet(cluster, false))
		maxUnavailable := intstr.FromInt(int(cluster.Spec.Replicas / 2))
		pdb.Spec.MaxUnavailable = &maxUnavailable
		pdb.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: labelSet(cluster, false),
		}
		return ctrl.SetControllerReference(cluster, pdb, r.Scheme)
	})
	if err != nil {
		log.Error(err, "failed to reconcile pod disruption budget")
		return err
	}
	if result != controllerutil.OperationResultNone {
		log.Info("reconciled pod disruption budget", "operation", string(result))
	}

	return nil
}

func bucketArgs(bc mocov1beta2.BucketConfig) []string {
	var args []string
	if bc.Region != "" {
		args = append(args, "--region="+bc.Region)
	}
	if bc.EndpointURL != "" {
		args = append(args, "--endpoint="+bc.EndpointURL)
	}
	if bc.UsePathStyle {
		args = append(args, "--use-path-style")
	}
	return append(args, bc.BucketName)
}

func (r *MySQLClusterReconciler) reconcileV1BackupJob(ctx context.Context, req ctrl.Request, cluster *mocov1beta2.MySQLCluster) error {
	log := crlog.FromContext(ctx)

	if cluster.Spec.BackupPolicyName == nil {
		cj := &batchv1beta1.CronJob{}
		err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.BackupCronJobName()}, cj)
		if err == nil {
			if err := r.Delete(ctx, cj); err != nil {
				log.Error(err, "failed to delete CronJob")
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}

		role := &rbacv1.Role{}
		err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.BackupRoleName()}, role)
		if err == nil {
			if err := r.Delete(ctx, role); err != nil {
				log.Error(err, "failed to delete Role")
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		rolebinding := &rbacv1.RoleBinding{}
		err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.BackupRoleName()}, rolebinding)
		if err == nil {
			if err := r.Delete(ctx, rolebinding); err != nil {
				log.Error(err, "failed to delete RoleBinding")
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}

		return nil
	}

	bpName := *cluster.Spec.BackupPolicyName
	bp := &mocov1beta2.BackupPolicy{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: bpName}, bp); err != nil {
		return fmt.Errorf("failed to get backup policy %s/%s: %w", cluster.Namespace, bpName, err)
	}

	cj := &batchv1beta1.CronJob{}
	cj.Namespace = cluster.Namespace
	cj.Name = cluster.BackupCronJobName()
	var orig, updated *batchv1beta1.CronJobSpec
	result, err := ctrl.CreateOrUpdate(ctx, r.Client, cj, func() error {
		if debugController {
			orig = cj.Spec.DeepCopy()
		}

		cj.Labels = mergeMap(cj.Labels, labelSetForJob(cluster))
		cj.Spec.Schedule = bp.Spec.Schedule
		cj.Spec.StartingDeadlineSeconds = bp.Spec.StartingDeadlineSeconds
		cj.Spec.ConcurrencyPolicy = bp.Spec.ConcurrencyPolicy
		cj.Spec.SuccessfulJobsHistoryLimit = bp.Spec.SuccessfulJobsHistoryLimit
		cj.Spec.FailedJobsHistoryLimit = bp.Spec.FailedJobsHistoryLimit
		cj.Spec.JobTemplate.Labels = labelSetForJob(cluster)
		cj.Spec.JobTemplate.Spec.ActiveDeadlineSeconds = bp.Spec.ActiveDeadlineSeconds
		cj.Spec.JobTemplate.Spec.BackoffLimit = bp.Spec.BackoffLimit
		cj.Spec.JobTemplate.Spec.Template.Labels = labelSetForJob(cluster)
		podSpec := &cj.Spec.JobTemplate.Spec.Template.Spec
		jc := &bp.Spec.JobConfig
		podSpec.RestartPolicy = corev1.RestartPolicyNever
		podSpec.ServiceAccountName = jc.ServiceAccountName
		podSpec.Volumes = []corev1.Volume{{
			Name:         "work",
			VolumeSource: *jc.WorkVolume.DeepCopy(),
		}}

		args := []string{constants.BackupSubcommand, fmt.Sprintf("--threads=%d", jc.Threads)}
		args = append(args, bucketArgs(jc.BucketConfig)...)
		args = append(args, cluster.Namespace, cluster.Name)
		env := []corev1.EnvVar{
			{Name: "MYSQL_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: cluster.UserSecretName()},
				Key:                  password.BackupPasswordKey,
			}}},
		}
		res := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewQuantity(int64(jc.Threads), resource.DecimalSI),
			},
		}
		if jc.Memory != nil {
			res.Requests[corev1.ResourceMemory] = *jc.Memory
		} else {
			delete(res.Requests, corev1.ResourceMemory)
		}
		if jc.MaxMemory != nil {
			res.Limits = corev1.ResourceList{corev1.ResourceMemory: *jc.MaxMemory}
		} else {
			delete(res.Limits, corev1.ResourceMemory)
		}
		if noJobResource {
			res = corev1.ResourceRequirements{}
		}

		c := corev1.Container{
			Name:            "backup",
			Image:           r.BackupImage,
			Args:            args,
			EnvFrom:         append([]corev1.EnvFromSource{}, jc.EnvFrom...),
			Env:             append(env, jc.Env...),
			VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: "/work"}},
			SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: pointer.Bool(true)},
			Resources:       res,
		}
		updateContainerWithSupplements(&c, podSpec.Containers)
		podSpec.Containers = []corev1.Container{c}

		if debugController {
			updated = cj.Spec.DeepCopy()
		}

		return ctrl.SetControllerReference(cluster, cj, r.Scheme)
	})
	if err != nil {
		log.Error(err, "failed to reconcile CronJob for backup")
		return err
	}
	if result != controllerutil.OperationResultNone {
		log.Info("reconciled CronJob for backup", "operation", string(result))
	}
	if result == controllerutil.OperationResultUpdated && debugController {
		fmt.Println(cmp.Diff(orig, updated))
	}

	role := &rbacv1.Role{}
	role.Namespace = cluster.Namespace
	role.Name = cluster.BackupRoleName()
	result, err = ctrl.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = mergeMap(role.Labels, labelSetForJob(cluster))
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{mocov1beta2.GroupVersion.Group},
				Resources:     []string{"mysqlclusters", "mysqlclusters/status"},
				Verbs:         []string{"get", "update"},
				ResourceNames: []string{cluster.Name},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create", "update", "patch"},
			},
		}
		return ctrl.SetControllerReference(cluster, role, r.Scheme)
	})
	if err != nil {
		log.Error(err, "failed to reconcile Role for backup")
		return err
	}
	if result != controllerutil.OperationResultNone {
		log.Info("reconciled Role for backup", "operation", string(result))
	}

	rolebinding := &rbacv1.RoleBinding{}
	rolebinding.Namespace = cluster.Namespace
	rolebinding.Name = cluster.BackupRoleName()
	result, err = ctrl.CreateOrUpdate(ctx, r.Client, rolebinding, func() error {
		rolebinding.Labels = mergeMap(rolebinding.Labels, labelSetForJob(cluster))
		rolebinding.RoleRef.APIGroup = rbacv1.SchemeGroupVersion.Group
		rolebinding.RoleRef.Kind = "Role"
		rolebinding.RoleRef.Name = cluster.BackupRoleName()
		rolebinding.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      bp.Spec.JobConfig.ServiceAccountName,
			Namespace: cluster.Namespace,
		}}
		return ctrl.SetControllerReference(cluster, rolebinding, r.Scheme)
	})
	if err != nil {
		log.Error(err, "failed to reconcile RoleBinding for backup")
		return err
	}
	if result != controllerutil.OperationResultNone {
		log.Info("reconciled RoleBinding for backup", "operation", string(result))
	}

	return nil
}

func (r *MySQLClusterReconciler) reconcileV1RestoreJob(ctx context.Context, req ctrl.Request, cluster *mocov1beta2.MySQLCluster) error {
	// `spec.restore` is not editable, so we can safely return early if it is nil.
	if cluster.Spec.Restore == nil {
		return nil
	}
	// the restoration has already finished successfully.
	if cluster.Status.RestoredTime != nil {
		return nil
	}

	log := crlog.FromContext(ctx)

	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.RestoreJobName()}, job)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}

		job = &batchv1.Job{}
		job.Namespace = cluster.Namespace
		job.Name = cluster.RestoreJobName()
		job.Labels = labelSetForJob(cluster)
		if err := ctrl.SetControllerReference(cluster, job, r.Scheme); err != nil {
			return fmt.Errorf("failed to set controller reference to restore Job: %w", err)
		}
		job.Spec.BackoffLimit = pointer.Int32(0)
		job.Spec.Template.Labels = labelSetForJob(cluster)
		podSpec := &job.Spec.Template.Spec
		jc := &cluster.Spec.Restore.JobConfig
		podSpec.RestartPolicy = corev1.RestartPolicyNever
		podSpec.ServiceAccountName = jc.ServiceAccountName
		podSpec.Volumes = []corev1.Volume{{
			Name:         "work",
			VolumeSource: *jc.WorkVolume.DeepCopy(),
		}}

		args := []string{constants.RestoreSubcommand, fmt.Sprintf("--threads=%d", jc.Threads)}
		args = append(args, bucketArgs(jc.BucketConfig)...)
		args = append(args, cluster.Spec.Restore.SourceNamespace, cluster.Spec.Restore.SourceName)
		args = append(args, cluster.Namespace, cluster.Name)
		args = append(args, cluster.Spec.Restore.RestorePoint.UTC().Format(constants.BackupTimeFormat))
		env := []corev1.EnvVar{
			{Name: "MYSQL_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: cluster.UserSecretName()},
				Key:                  password.AdminPasswordKey,
			}}},
		}
		res := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewQuantity(int64(jc.Threads), resource.DecimalSI),
			},
		}
		if jc.Memory != nil {
			res.Requests[corev1.ResourceMemory] = *jc.Memory
		} else {
			delete(res.Requests, corev1.ResourceMemory)
		}
		if jc.MaxMemory != nil {
			res.Limits = corev1.ResourceList{corev1.ResourceMemory: *jc.MaxMemory}
		} else {
			delete(res.Limits, corev1.ResourceMemory)
		}
		if noJobResource {
			res = corev1.ResourceRequirements{}
		}

		c := corev1.Container{
			Name:            "restore",
			Image:           r.BackupImage,
			Args:            args,
			EnvFrom:         append([]corev1.EnvFromSource{}, jc.EnvFrom...),
			Env:             append(env, jc.Env...),
			VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: "/work"}},
			SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: pointer.Bool(true)},
			Resources:       res,
		}
		podSpec.Containers = []corev1.Container{c}

		if err := r.Create(ctx, job); err != nil {
			log.Error(err, "failed to create a restore Job: %w", err)
			return err
		}

		log.Info("reconciled restore Job")
	}

	role := &rbacv1.Role{}
	role.Namespace = cluster.Namespace
	role.Name = cluster.RestoreRoleName()
	result, err := ctrl.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = mergeMap(role.Labels, labelSetForJob(cluster))
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{mocov1beta2.GroupVersion.Group},
				Resources:     []string{"mysqlclusters", "mysqlclusters/status"},
				Verbs:         []string{"get", "update"},
				ResourceNames: []string{cluster.Name},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create"},
			},
		}
		return ctrl.SetControllerReference(job, role, r.Scheme)
	})
	if err != nil {
		log.Error(err, "failed to reconcile Role for restore")
		return err
	}
	if result != controllerutil.OperationResultNone {
		log.Info("reconciled Role for restore", "operation", string(result))
	}

	rolebinding := &rbacv1.RoleBinding{}
	rolebinding.Namespace = cluster.Namespace
	rolebinding.Name = cluster.RestoreRoleName()
	result, err = ctrl.CreateOrUpdate(ctx, r.Client, rolebinding, func() error {
		rolebinding.Labels = mergeMap(rolebinding.Labels, labelSetForJob(cluster))
		rolebinding.RoleRef.APIGroup = rbacv1.SchemeGroupVersion.Group
		rolebinding.RoleRef.Kind = "Role"
		rolebinding.RoleRef.Name = cluster.RestoreRoleName()
		rolebinding.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      cluster.Spec.Restore.JobConfig.ServiceAccountName,
			Namespace: cluster.Namespace,
		}}
		return ctrl.SetControllerReference(job, rolebinding, r.Scheme)
	})
	if err != nil {
		log.Error(err, "failed to reconcile RoleBinding for restore")
		return err
	}
	if result != controllerutil.OperationResultNone {
		log.Info("reconciled RoleBinding for restore", "operation", string(result))
	}

	return nil
}

func (r *MySQLClusterReconciler) finalizeV1(ctx context.Context, cluster *mocov1beta2.MySQLCluster) error {
	secretName := cluster.ControllerSecretName()
	secret := &corev1.Secret{}
	secret.SetNamespace(r.SystemNamespace)
	secret.SetName(secretName)
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete controller secret %s: %w", secretName, err)
	}

	certName := cluster.CertificateName()
	cert := certificateObj.DeepCopy()
	cert.SetNamespace(r.SystemNamespace)
	cert.SetName(certName)
	if err := r.Delete(ctx, cert); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete certificate %s: %w", certName, err)
	}

	return nil
}

func setControllerReferenceWithService(cluster *mocov1beta2.MySQLCluster, svc *corev1ac.ServiceApplyConfiguration, scheme *runtime.Scheme) error {
	gvk, err := apiutil.GVKForObject(cluster, scheme)
	if err != nil {
		return err
	}
	svc.WithOwnerReferences(metav1ac.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(cluster.Name).
		WithUID(cluster.GetUID()).
		WithBlockOwnerDeletion(true).
		WithController(true))
	return nil
}

func setControllerReferenceWithStatefulSet(cluster *mocov1beta2.MySQLCluster, sts *appsv1ac.StatefulSetApplyConfiguration, scheme *runtime.Scheme) error {
	gvk, err := apiutil.GVKForObject(cluster, scheme)
	if err != nil {
		return err
	}
	sts.WithOwnerReferences(metav1ac.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(cluster.Name).
		WithUID(cluster.GetUID()).
		WithBlockOwnerDeletion(true).
		WithController(true))
	return nil
}

func setControllerReferenceWithPVC(cluster *mocov1beta2.MySQLCluster, pvc *corev1ac.PersistentVolumeClaimApplyConfiguration, scheme *runtime.Scheme) error {
	gvk, err := apiutil.GVKForObject(cluster, scheme)
	if err != nil {
		return err
	}
	pvc.WithOwnerReferences(metav1ac.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(cluster.Name).
		WithUID(cluster.GetUID()).
		WithBlockOwnerDeletion(true).
		WithController(true))
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MySQLClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	certHandler := handler.EnqueueRequestsFromMapFunc(func(a client.Object) []reconcile.Request {
		// the certificate name is formatted as "moco-agent-<cluster.Namespace>.<cluster.Name>"
		if a.GetNamespace() != r.SystemNamespace {
			return nil
		}

		name := a.GetName()
		if !strings.HasPrefix(name, "moco-agent-") {
			return nil
		}
		fields := strings.SplitN(name[len("moco-agent-"):], ".", 2)
		if len(fields) != 2 {
			return nil
		}
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Namespace: fields[0], Name: fields[1]}},
		}
	})

	configMapHandler := handler.EnqueueRequestsFromMapFunc(func(a client.Object) []reconcile.Request {
		clusters := &mocov1beta2.MySQLClusterList{}
		if err := r.List(context.Background(), clusters, client.InNamespace(a.GetNamespace())); err != nil {
			return nil
		}
		var req []reconcile.Request
		for _, c := range clusters.Items {
			if c.Spec.MySQLConfigMapName == nil {
				continue
			}
			if *c.Spec.MySQLConfigMapName == a.GetName() {
				req = append(req, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&c)})
			}
		}
		return req
	})

	backupPolicyHandler := handler.EnqueueRequestsFromMapFunc(func(a client.Object) []reconcile.Request {
		clusters := &mocov1beta2.MySQLClusterList{}
		if err := r.List(context.Background(), clusters, client.InNamespace(a.GetNamespace())); err != nil {
			return nil
		}
		var req []reconcile.Request
		for _, c := range clusters.Items {
			if c.Spec.BackupPolicyName == nil {
				continue
			}
			if *c.Spec.BackupPolicyName == a.GetName() {
				req = append(req, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&c)})
			}
		}
		return req
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&mocov1beta2.MySQLCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1beta1.PodDisruptionBudget{}).
		Owns(&batchv1beta1.CronJob{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&batchv1.Job{}).
		Watches(&source.Kind{Type: certificateObj}, certHandler).
		Watches(&source.Kind{Type: &corev1.ConfigMap{}}, configMapHandler).
		Watches(&source.Kind{Type: &mocov1beta2.BackupPolicy{}}, backupPolicyHandler).
		WithOptions(
			controller.Options{MaxConcurrentReconciles: 8},
		).
		Complete(r)
}
