package controllers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cybozu-go/moco"
	mocov1alpha1 "github.com/cybozu-go/moco/api/v1alpha1"
	"github.com/cybozu-go/moco/runners"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	myName          = "moco"
	appNameKey      = "app.kubernetes.io/name"
	appManagedByKey = "app.kubernetes.io/managed-by"

	mysqldContainerName = "mysqld"
	mysqlPort           = 3306
	mysqlAdminPort      = 33062
	mysqlxPort          = 33060

	rotateServerContainerName   = "rotate"
	entrypointInitContainerName = "moco-init"
	confInitContainerName       = "moco-conf-gen"

	mysqlDataVolumeName         = "mysql-data"
	mysqlConfVolumeName         = "mysql-conf"
	varRunVolumeName            = "var-run"
	varLogVolumeName            = "var-log"
	tmpVolumeName               = "tmp"
	mysqlConfTemplateVolumeName = "mysql-conf-template"

	passwordBytes = 16

	defaultTerminationGracePeriodSeconds = 300

	mysqlClusterFinalizer = "moco.cybozu.com/mysqlcluster"

	rootPasswordSecretPrefix = "root-password-"
	serviceAccountPrefix     = "mysqld-sa-"
)

// MySQLClusterReconciler reconciles a MySQLCluster object
type MySQLClusterReconciler struct {
	client.Client
	Log                    logr.Logger
	Scheme                 *runtime.Scheme
	ConfInitContainerImage string
	CurlContainerImage     string
	MySQLAccessor          *MySQLAccessor
}

// +kubebuilder:rbac:groups=moco.cybozu.com,resources=mysqlclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=moco.cybozu.com,resources=mysqlclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets/status,verbs=get
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services/status,verbs=get
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts/status,verbs=get
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get
// +kubebuilder:rbac:groups="batch",resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="batch",resources=cronjobs/status,verbs=get

// Reconcile reconciles MySQLCluster.
func (r *MySQLClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("mysqlcluster", req.NamespacedName)

	cluster := &mocov1alpha1.MySQLCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		log.Error(err, "unable to fetch MySQLCluster", "name", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if cluster.DeletionTimestamp == nil {
		if !containsString(cluster.Finalizers, mysqlClusterFinalizer) {
			cluster2 := cluster.DeepCopy()
			cluster2.Finalizers = append(cluster2.Finalizers, mysqlClusterFinalizer)
			patch := client.MergeFrom(cluster)
			if err := r.Patch(ctx, cluster2, patch); err != nil {
				log.Error(err, "failed to add finalizer", "name", cluster.Name)
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		// initialize
		isUpdated, err := r.reconcileInitialize(ctx, log, cluster)
		if err != nil {
			setCondition(&cluster.Status.Conditions, mocov1alpha1.MySQLClusterCondition{
				Type: mocov1alpha1.ConditionInitialized, Status: corev1.ConditionFalse, Reason: "reconcileInitializeFailed", Message: err.Error()})
			if errUpdate := r.Status().Update(ctx, cluster); errUpdate != nil {
				log.Error(err, "failed to status update")
			}
			log.Error(err, "failed to initialize MySQLCluster")
			return ctrl.Result{}, err
		}
		if isUpdated {
			setCondition(&cluster.Status.Conditions, mocov1alpha1.MySQLClusterCondition{
				Type: mocov1alpha1.ConditionInitialized, Status: corev1.ConditionTrue})
			if err := r.Status().Update(ctx, cluster); err != nil {
				log.Error(err, "failed to status update")
				return ctrl.Result{}, err
			}
		}

		// clustering
		isUpdated, err = r.reconcileClustering(ctx, log, cluster)
		if err != nil {
			setCondition(&cluster.Status.Conditions, mocov1alpha1.MySQLClusterCondition{
				Type: mocov1alpha1.ConditionReady, Status: corev1.ConditionFalse, Reason: "reconcileClusteringFailed", Message: err.Error()})
			if errUpdate := r.Status().Update(ctx, cluster); errUpdate != nil {
				log.Error(err, "failed to status update")
			}
			log.Error(err, "failed to ready MySQLCluster")
			return ctrl.Result{}, err
		}
		if isUpdated {
			setCondition(&cluster.Status.Conditions, mocov1alpha1.MySQLClusterCondition{
				Type: mocov1alpha1.ConditionReady, Status: corev1.ConditionTrue})
			if err := r.Status().Update(ctx, cluster); err != nil {
				log.Error(err, "failed to status update")
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, nil
	}

	// finalization
	if !containsString(cluster.Finalizers, mysqlClusterFinalizer) {
		// Our finalizer has finished, so the reconciler can do nothing.
		return ctrl.Result{}, nil
	}

	log.Info("start finalizing MySQLCluster", "name", cluster.Name)
	err := r.removePasswordSecretForController(ctx, log, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	cluster2 := cluster.DeepCopy()
	cluster2.Finalizers = removeString(cluster2.Finalizers, mysqlClusterFinalizer)
	patch := client.MergeFrom(cluster)
	if err := r.Patch(ctx, cluster2, patch); err != nil {
		log.Error(err, "failed to remove finalizer", "name", cluster.Name)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *MySQLClusterReconciler) reconcileInitialize(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (bool, error) {

	isUpdatedAtLeastOnce := false
	isUpdated, err := r.createSecretIfNotExist(ctx, log, cluster)
	isUpdatedAtLeastOnce = isUpdatedAtLeastOnce || isUpdated
	if err != nil {
		return false, err
	}

	isUpdated, err = r.createOrUpdateConfigMap(ctx, log, cluster)
	isUpdatedAtLeastOnce = isUpdatedAtLeastOnce || isUpdated
	if err != nil {
		return false, err
	}

	isUpdated, err = r.createOrUpdateHeadlessService(ctx, log, cluster)
	isUpdatedAtLeastOnce = isUpdatedAtLeastOnce || isUpdated
	if err != nil {
		return false, err
	}

	isUpdated, err = r.createOrUpdateRBAC(ctx, log, cluster)
	isUpdatedAtLeastOnce = isUpdatedAtLeastOnce || isUpdated
	if err != nil {
		return false, err
	}

	isUpdated, err = r.createOrUpdateStatefulSet(ctx, log, cluster)
	isUpdatedAtLeastOnce = isUpdatedAtLeastOnce || isUpdated
	if err != nil {
		return false, err
	}

	isUpdated, err = r.createOrUpdateCronJob(ctx, log, cluster)
	isUpdatedAtLeastOnce = isUpdatedAtLeastOnce || isUpdated
	if err != nil {
		return false, err
	}

	return isUpdatedAtLeastOnce, nil
}

// SetupWithManager sets up the controller for reconciliation.
func (r *MySQLClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// SetupWithManager sets up the controller for reconciliation.

	err := mgr.GetFieldIndexer().IndexField(&mocov1alpha1.MySQLCluster{}, moco.InitializedClusterIndexField, selectInitializedCluster)
	if err != nil {
		return err
	}

	ch := make(chan event.GenericEvent)
	watcher := runners.NewMySQLClusterWatcher(mgr.GetClient(), ch)
	err = mgr.Add(watcher)
	if err != nil {
		return err
	}
	src := source.Channel{
		Source: ch,
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&mocov1alpha1.MySQLCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&batchv1beta1.CronJob{}).
		Watches(&src, &handler.EnqueueRequestForObject{}).
		WithOptions(
			controller.Options{MaxConcurrentReconciles: 8},
		).
		Complete(r)
}

func selectInitializedCluster(obj runtime.Object) []string {
	cluster := obj.(*mocov1alpha1.MySQLCluster)
	if cluster.Status == nil {
		return []string{string(corev1.ConditionUnknown)}
	}

	for _, cond := range cluster.Status.Conditions {
		if cond.Type == mocov1alpha1.ConditionInitialized {
			return []string{string(cond.Status)}
		}
	}
	return []string{string(corev1.ConditionUnknown)}
}

// reconcileMySQLCluster recoclies MySQL cluster
func (r *MySQLClusterReconciler) reconcileClustering(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (bool, error) {
	_, err := r.getMySQLClusterStatus(ctx, log, cluster)
	return true, err
}

// MySQLClusterStatus contains MySQLCluster status
type MySQLClusterStatus struct {
	InstanceStatus []MySQLInstanceStatus
}

type MySQLPrimaryStatus struct {
	ExecutedGtidSet string
}

type MySQLReplicaStatus struct {
	PrimaryHost       string
	ReplicaIORunning  string
	ReplicaSQLRunning string
	RetrievedGtidSet  string
	ExecutedGtidSet   string
}

type MySQLInstanceStatus struct {
	Available     bool
	PrimaryStatus *MySQLPrimaryStatus
	ReplicaStatus []MySQLReplicaStatus
}

func (r *MySQLClusterReconciler) getMySQLClusterStatus(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (*MySQLClusterStatus, error) {
	secret := &corev1.Secret{}
	myNS, mySecretName := r.getSecretNameForController(cluster)
	err := r.Get(ctx, client.ObjectKey{Namespace: myNS, Name: mySecretName}, secret)
	if err != nil {
		return nil, err
	}
	operatorPassword := string(secret.Data[moco.OperatorPasswordKey])

	status := &MySQLClusterStatus{
		InstanceStatus: make([]MySQLInstanceStatus, int(cluster.Spec.Replicas)),
	}
	for instanceIdx := 0; instanceIdx < int(cluster.Spec.Replicas); instanceIdx++ {
		status.InstanceStatus[instanceIdx].Available = false

		podName := fmt.Sprintf("%s-%d", uniqueName(cluster), instanceIdx)
		host := fmt.Sprintf("%s.%s.%s", podName, uniqueName(cluster), cluster.Namespace)

		db, err := r.MySQLAccessor.Get(host, moco.OperatorAdminUser, operatorPassword)
		if err != nil {
			log.Info("instance not available", "err", err, "podName", podName)
			continue
		}

		primaryStatus, err := r.getMySQLPrimaryStatus(ctx, log, db)
		if err != nil {
			log.Info("get primary status failed", "err", err, "podName", podName)
			continue
		}
		status.InstanceStatus[instanceIdx].PrimaryStatus = primaryStatus

		replicaStatus, err := r.getMySQLReplicaStatus(ctx, log, db)
		if err != nil {
			log.Info("get replica status failed", "err", err, "podName", podName)
			continue
		}
		status.InstanceStatus[instanceIdx].ReplicaStatus = replicaStatus

		status.InstanceStatus[instanceIdx].Available = true
	}
	return status, nil
}

func (r *MySQLClusterReconciler) getMySQLPrimaryStatus(ctx context.Context, log logr.Logger, db *sql.DB) (*MySQLPrimaryStatus, error) {
	rows, err := r.getColumns(ctx, log, db, "SHOW MASTER STATUS")
	if err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		return nil, nil
	}

	if len(rows) != 1 {
		return nil, fmt.Errorf("unsupported topology")
	}

	status := &MySQLPrimaryStatus{
		ExecutedGtidSet: rows[0]["Executed_Gtid_Set"],
	}

	return status, nil
}

func (r *MySQLClusterReconciler) getMySQLReplicaStatus(ctx context.Context, log logr.Logger, db *sql.DB) ([]MySQLReplicaStatus, error) {
	rows, err := r.getColumns(ctx, log, db, "SHOW SLAVE STATUS")
	if err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		return nil, nil
	}

	status := make([]MySQLReplicaStatus, len(rows))
	for idx, row := range rows {
		status[idx] = MySQLReplicaStatus{
			PrimaryHost:       row["Slave_IO_State"],
			ReplicaIORunning:  row["Slave_IO_Running"],
			ReplicaSQLRunning: row["Slave_SQL_Running"],
			RetrievedGtidSet:  row["Retrieved_Gtid_Set"],
			ExecutedGtidSet:   row["Executed_Gtid_Set"],
		}
	}
	return status, nil
}

func (r *MySQLClusterReconciler) getColumns(ctx context.Context, log logr.Logger, db *sql.DB, query string) ([]map[string]string, error) {
	rows, err := db.Query(query)
	if rows != nil {
		defer rows.Close()
	}
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	// Get column names
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	// Make a slice for the values
	values := make([]sql.RawBytes, len(columns))

	// rows.Scan wants '[]interface{}' as an argument, so we must copy the
	// references into such a slice
	// See http://code.google.com/p/go-wiki/wiki/InterfaceSlice for details
	scanArgs := make([]interface{}, len(values))
	for i := range values {
		scanArgs[i] = &values[i]
	}

	result := make([]map[string]string, 0)

	// Fetch rows
	for rows.Next() {
		row := make(map[string]string)
		result = append(result, row)
		// get RawBytes from data
		err = rows.Scan(scanArgs...)
		if err != nil {
			return nil, err
		}

		// Now do something with the data.
		// Here we just print each column as a string.
		var value string
		for i, col := range values {
			// Here we can check if the value is nil (NULL value)
			if col == nil {
				value = "NULL"
			} else {
				value = string(col)
			}
			row[columns[i]] = value
		}
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

func (r *MySQLClusterReconciler) createSecretIfNotExist(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (bool, error) {
	secret := &corev1.Secret{}
	myNS, mySecretName := r.getSecretNameForController(cluster)
	err := r.Get(ctx, client.ObjectKey{Namespace: myNS, Name: mySecretName}, secret)
	if err == nil {
		return false, nil
	}
	if !errors.IsNotFound(err) {
		log.Error(err, "unable to get Secret")
		return false, err
	}

	operatorPass, err := generateRandomBytes(passwordBytes)
	if err != nil {
		return false, err
	}
	replicatorPass, err := generateRandomBytes(passwordBytes)
	if err != nil {
		return false, err
	}
	donorPass, err := generateRandomBytes(passwordBytes)
	if err != nil {
		return false, err
	}

	err = r.createPasswordSecretForInit(ctx, cluster, operatorPass, replicatorPass, donorPass)
	if err != nil {
		log.Error(err, "unable to create Secret for user")
		return false, err
	}

	// Secret for controller must be created lastly, because its existence is checked at the beginning of the process
	err = r.createPasswordSecretForController(ctx, myNS, mySecretName, operatorPass, replicatorPass, donorPass)
	if err != nil {
		log.Error(err, "unable to create Secret for Controller")
		return false, err
	}

	return true, nil
}

func (r *MySQLClusterReconciler) createPasswordSecretForInit(ctx context.Context, cluster *mocov1alpha1.MySQLCluster, operatorPass, replicatorPass, donorPass []byte) error {
	var rootPass []byte
	if cluster.Spec.RootPasswordSecretName != nil {
		secret := &corev1.Secret{}
		err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: *cluster.Spec.RootPasswordSecretName}, secret)
		if err != nil {
			return err
		}
		rootPass = secret.Data[moco.RootPasswordKey]
	}
	if len(rootPass) == 0 {
		var err error
		rootPass, err = generateRandomBytes(passwordBytes)
		if err != nil {
			return err
		}
	}
	miscPass, err := generateRandomBytes(passwordBytes)
	if err != nil {
		return err
	}
	secretName := rootPasswordSecretPrefix + uniqueName(cluster)
	secret := &corev1.Secret{}
	secret.SetNamespace(cluster.Namespace)
	secret.SetName(secretName)

	setLabels(&secret.ObjectMeta)

	secret.Data = map[string][]byte{
		moco.RootPasswordKey:        rootPass,
		moco.OperatorPasswordKey:    operatorPass,
		moco.ReplicationPasswordKey: replicatorPass,
		moco.DonorPasswordKey:       donorPass,
		moco.MiscPasswordKey:        miscPass,
	}

	err = ctrl.SetControllerReference(cluster, secret, r.Scheme)
	if err != nil {
		return err
	}

	return r.Client.Create(ctx, secret)
}

func (r *MySQLClusterReconciler) createPasswordSecretForController(ctx context.Context, namespace, secretName string, operatorPass, replicatorPass, donorPass []byte) error {
	secret := &corev1.Secret{}
	secret.SetNamespace(namespace)
	secret.SetName(secretName)

	secret.Data = map[string][]byte{
		moco.OperatorPasswordKey:    operatorPass,
		moco.ReplicationPasswordKey: replicatorPass,
		moco.DonorPasswordKey:       donorPass,
	}

	return r.Client.Create(ctx, secret)
}

func (r *MySQLClusterReconciler) removePasswordSecretForController(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) error {
	secret := &corev1.Secret{}
	myNS, mySecretName := r.getSecretNameForController(cluster)
	err := r.Get(ctx, client.ObjectKey{Namespace: myNS, Name: mySecretName}, secret)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		log.Error(err, "unable to get Secret")
		return err
	}
	err = r.Delete(ctx, secret)
	if err != nil {
		log.Error(err, "unable to delete Secret")
		return err
	}
	return nil
}

func (r *MySQLClusterReconciler) getSecretNameForController(cluster *mocov1alpha1.MySQLCluster) (string, string) {
	myNS := os.Getenv("POD_NAMESPACE")
	mySecretName := cluster.Namespace + "." + cluster.Name // TODO: clarify assumptions for length and charset
	return myNS, mySecretName
}

func generateRandomBytes(n int) ([]byte, error) {
	bytes := make([]byte, n)
	_, err := rand.Read(bytes)
	if err != nil {
		return nil, err
	}

	ret := make([]byte, hex.EncodedLen(n))
	hex.Encode(ret, bytes)
	return ret, nil
}

func (r *MySQLClusterReconciler) createOrUpdateConfigMap(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (bool, error) {
	cm := &corev1.ConfigMap{}
	cm.SetNamespace(cluster.Namespace)
	cm.SetName(uniqueName(cluster))

	op, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		setLabels(&cm.ObjectMeta)
		gen := mysqlConfGenerator{
			log: log,
		}
		gen.mergeSection("mysqld", defaultMycnf, false)

		// Set innodb_buffer_pool_size if resources.requests.memory or resources.limits.memory is specified
		mem := getMysqldContainerRequests(cluster, corev1.ResourceMemory)
		// 128MiB is the default innodb_buffer_pool_size value
		if mem != nil && mem.Value() > (128<<20) {
			bufferSize := mem.Value() / 10 * 7
			gen.mergeSection("mysqld", map[string]string{"innodb_buffer_pool_size": fmt.Sprintf("%dM", bufferSize>>20)}, false)
		}

		if cluster.Spec.MySQLConfigMapName != nil {
			cm := &corev1.ConfigMap{}
			err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: *cluster.Spec.MySQLConfigMapName}, cm)
			if err != nil {
				return err
			}
			gen.mergeSection("mysqld", cm.Data, false)
		}

		gen.merge(constMycnf, true)

		myCnf, err := gen.generate()
		if err != nil {
			return err
		}
		cm.Data = make(map[string]string)
		cm.Data[moco.MySQLConfName] = myCnf

		return ctrl.SetControllerReference(cluster, cm, r.Scheme)
	})
	if err != nil {
		log.Error(err, "unable to create-or-update ConfigMap")
		return false, err
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconcile ConfigMap successfully", "op", op)
		return true, nil
	}
	return false, nil
}

func (r *MySQLClusterReconciler) createOrUpdateHeadlessService(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (bool, error) {
	headless := &corev1.Service{}
	headless.SetNamespace(cluster.Namespace)
	headless.SetName(uniqueName(cluster))

	op, err := ctrl.CreateOrUpdate(ctx, r.Client, headless, func() error {
		setLabels(&headless.ObjectMeta)
		headless.Spec.ClusterIP = corev1.ClusterIPNone
		headless.Spec.Selector = map[string]string{
			appNameKey:      uniqueName(cluster),
			appManagedByKey: myName,
		}
		return ctrl.SetControllerReference(cluster, headless, r.Scheme)
	})
	if err != nil {
		log.Error(err, "unable to create-or-update headless Service")
		return false, err
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconcile headless Service successfully", "op", op)
		return true, nil
	}
	return false, nil
}

func (r *MySQLClusterReconciler) createOrUpdateRBAC(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (bool, error) {
	if cluster.Spec.PodTemplate.Spec.ServiceAccountName != "" {
		return false, nil
	}

	saName := serviceAccountPrefix + uniqueName(cluster)
	sa := &corev1.ServiceAccount{}
	sa.SetNamespace(cluster.Namespace)
	sa.SetName(saName)

	op, err := ctrl.CreateOrUpdate(ctx, r.Client, sa, func() error {
		setLabels(&sa.ObjectMeta)
		return ctrl.SetControllerReference(cluster, sa, r.Scheme)
	})

	if err != nil {
		log.Error(err, "unable to create-or-update ServiceAccount")
		return false, err
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconcile ServiceAccount successfully", "op", op)
		return true, nil
	}
	return false, nil
}

func (r *MySQLClusterReconciler) createOrUpdateStatefulSet(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (bool, error) {
	sts := &appsv1.StatefulSet{}
	sts.SetNamespace(cluster.Namespace)
	sts.SetName(uniqueName(cluster))

	op, err := ctrl.CreateOrUpdate(ctx, r.Client, sts, func() error {
		setLabels(&sts.ObjectMeta)
		sts.Spec.Replicas = &cluster.Spec.Replicas
		sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
		sts.Spec.ServiceName = uniqueName(cluster)
		sts.Spec.Selector = &metav1.LabelSelector{}
		sts.Spec.Selector.MatchLabels = map[string]string{
			appNameKey:      uniqueName(cluster),
			appManagedByKey: myName,
		}
		template, err := r.makePodTemplate(log, cluster)
		if err != nil {
			return err
		}
		sts.Spec.Template = *template
		sts.Spec.VolumeClaimTemplates = append(
			r.makeVolumeClaimTemplates(cluster),
			r.makeDataVolumeClaimTemplate(cluster),
		)
		return ctrl.SetControllerReference(cluster, sts, r.Scheme)
	})
	if err != nil {
		log.Error(err, "unable to create-or-update StatefulSet")
		return false, err
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconcile StatefulSet successfully", "op", op)
		return true, nil
	}
	return false, nil
}

func (r *MySQLClusterReconciler) makePodTemplate(log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (*corev1.PodTemplateSpec, error) {
	template := cluster.Spec.PodTemplate
	newTemplate := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: template.Annotations,
		},
		Spec: template.Spec,
	}

	newTemplate.Labels = make(map[string]string)
	for k, v := range template.Labels {
		newTemplate.Labels[k] = v
	}

	// add labels to describe application
	if v, ok := newTemplate.Labels[appNameKey]; ok && v != myName {
		log.Info("overwriting Pod template's label", "label", appNameKey)
	}
	newTemplate.Labels[appNameKey] = uniqueName(cluster)
	newTemplate.Labels[appManagedByKey] = myName

	if newTemplate.Spec.ServiceAccountName != "" {
		log.Info("overwriting Pod template's serviceAccountName", "ServiceAccountName", newTemplate.Spec.ServiceAccountName)
	}
	newTemplate.Spec.ServiceAccountName = serviceAccountPrefix + uniqueName(cluster)

	if newTemplate.Spec.TerminationGracePeriodSeconds == nil {
		var t int64 = defaultTerminationGracePeriodSeconds
		newTemplate.Spec.TerminationGracePeriodSeconds = &t
	}

	// add volumes to Pod
	// If the original template contains volumes with the same names as below, CreateOrUpdate fails.
	newTemplate.Spec.Volumes = append(newTemplate.Spec.Volumes,
		corev1.Volume{
			Name: mysqlConfVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name: varRunVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name: varLogVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name: tmpVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name: mysqlConfTemplateVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: uniqueName(cluster),
					},
				},
			},
		},
	)

	// find "mysqld" container and update it
	var mysqldContainer *corev1.Container
	newTemplate.Spec.Containers = make([]corev1.Container, len(template.Spec.Containers))
	for i, orig := range template.Spec.Containers {
		if orig.Name != mysqldContainerName {
			newTemplate.Spec.Containers[i] = orig
			continue
		}

		c := orig.DeepCopy()
		c.Args = []string{"--defaults-file=" + filepath.Join(moco.MySQLConfPath, moco.MySQLConfName)}
		c.Ports = []corev1.ContainerPort{
			{
				ContainerPort: mysqlPort, Protocol: corev1.ProtocolTCP},
			{
				ContainerPort: mysqlxPort, Protocol: corev1.ProtocolTCP},
			{
				ContainerPort: mysqlAdminPort, Protocol: corev1.ProtocolTCP},
		}
		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{
				MountPath: moco.MySQLDataPath,
				Name:      mysqlDataVolumeName,
			},
			corev1.VolumeMount{
				MountPath: moco.MySQLConfPath,
				Name:      mysqlConfVolumeName,
			},
			corev1.VolumeMount{
				MountPath: moco.VarRunPath,
				Name:      varRunVolumeName,
			},
			corev1.VolumeMount{
				MountPath: moco.VarLogPath,
				Name:      varLogVolumeName,
			},
			corev1.VolumeMount{
				MountPath: moco.TmpPath,
				Name:      tmpVolumeName,
			},
		)
		newTemplate.Spec.Containers[i] = *c
		mysqldContainer = &newTemplate.Spec.Containers[i]
	}

	if mysqldContainer == nil {
		return nil, fmt.Errorf("container named %q not found in podTemplate", mysqldContainerName)
	}

	for _, orig := range template.Spec.Containers {
		if orig.Name == rotateServerContainerName {
			err := fmt.Errorf("cannot specify %s container in podTemplate", rotateServerContainerName)
			log.Error(err, "invalid container found")
			return nil, err
		}
	}
	newTemplate.Spec.Containers = append(newTemplate.Spec.Containers, corev1.Container{
		Name:  rotateServerContainerName,
		Image: mysqldContainer.Image,
		Command: []string{
			"/entrypoint", "rotate-server",
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				MountPath: moco.MySQLDataPath,
				Name:      mysqlDataVolumeName,
			},
			{
				MountPath: moco.MySQLConfPath,
				Name:      mysqlConfVolumeName,
			},
			{
				MountPath: moco.VarRunPath,
				Name:      varRunVolumeName,
			},
			{
				MountPath: moco.VarLogPath,
				Name:      varLogVolumeName,
			},
		},
	})

	// create init containers and append them to Pod
	newTemplate.Spec.InitContainers = append(newTemplate.Spec.InitContainers,
		r.makeConfInitContainer(log, cluster),
		r.makeEntrypointInitContainer(log, cluster, mysqldContainer.Image),
	)

	return &newTemplate, nil
}

func (r *MySQLClusterReconciler) makeConfInitContainer(log logr.Logger, cluster *mocov1alpha1.MySQLCluster) corev1.Container {
	c := corev1.Container{}
	c.Name = confInitContainerName

	c.Image = r.ConfInitContainerImage

	c.Command = []string{"/moco-conf-gen"}
	c.Env = append(c.Env,
		corev1.EnvVar{
			Name: moco.PodNameEnvName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		},
		corev1.EnvVar{
			Name: moco.PodNamespaceEnvName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		corev1.EnvVar{
			Name: moco.PodIPEnvName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
		corev1.EnvVar{
			Name: moco.NodeNameEnvName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "spec.nodeName",
				},
			},
		},
	)
	c.VolumeMounts = append(c.VolumeMounts,
		corev1.VolumeMount{
			MountPath: moco.MySQLConfPath,
			Name:      mysqlConfVolumeName,
		},
		corev1.VolumeMount{
			MountPath: moco.VarRunPath,
			Name:      varRunVolumeName,
		},
		corev1.VolumeMount{
			MountPath: moco.VarLogPath,
			Name:      varLogVolumeName,
		},
		corev1.VolumeMount{
			MountPath: moco.TmpPath,
			Name:      tmpVolumeName,
		},
		corev1.VolumeMount{
			MountPath: moco.MySQLConfTemplatePath,
			Name:      mysqlConfTemplateVolumeName,
		},
	)

	return c
}

func (r *MySQLClusterReconciler) makeEntrypointInitContainer(log logr.Logger, cluster *mocov1alpha1.MySQLCluster, mysqldContainerImage string) corev1.Container {
	c := corev1.Container{}
	c.Name = entrypointInitContainerName

	// use the same image with the 'mysqld' container
	c.Image = mysqldContainerImage

	c.Command = []string{"/entrypoint", "init"}
	secretName := rootPasswordSecretPrefix + uniqueName(cluster)
	c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: secretName,
			},
		},
	})
	c.Env = append(c.Env,
		corev1.EnvVar{
			Name: moco.PodIPEnvName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
	)
	c.VolumeMounts = append(c.VolumeMounts,
		corev1.VolumeMount{
			MountPath: moco.MySQLDataPath,
			Name:      mysqlDataVolumeName,
		},
		corev1.VolumeMount{
			MountPath: moco.MySQLConfPath,
			Name:      mysqlConfVolumeName,
		},
		corev1.VolumeMount{
			MountPath: moco.VarRunPath,
			Name:      varRunVolumeName,
		},
		corev1.VolumeMount{
			MountPath: moco.VarLogPath,
			Name:      varLogVolumeName,
		},
		corev1.VolumeMount{
			MountPath: moco.TmpPath,
			Name:      tmpVolumeName,
		},
	)

	return c
}

func (r *MySQLClusterReconciler) makeVolumeClaimTemplates(cluster *mocov1alpha1.MySQLCluster) []corev1.PersistentVolumeClaim {
	templates := cluster.Spec.VolumeClaimTemplates
	newTemplates := make([]corev1.PersistentVolumeClaim, len(templates))

	for i, template := range templates {
		newTemplates[i] = corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:        template.Name,
				Labels:      template.Labels,
				Annotations: template.Annotations,
			},
			Spec: template.Spec,
		}
	}

	return newTemplates
}

func (r *MySQLClusterReconciler) makeDataVolumeClaimTemplate(cluster *mocov1alpha1.MySQLCluster) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: mysqlDataVolumeName,
		},
		Spec: cluster.Spec.DataVolumeClaimTemplateSpec,
	}
}

// createOrUpdateCronJob doesn't remove cron jobs when the replica number is decreased
func (r *MySQLClusterReconciler) createOrUpdateCronJob(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (bool, error) {
	isUpdated := false
	for i := int32(0); i < cluster.Spec.Replicas; i++ {
		cronJob := &batchv1beta1.CronJob{}
		cronJob.SetNamespace(cluster.Namespace)
		podName := fmt.Sprintf("%s-%d", uniqueName(cluster), i)
		cronJob.SetName(podName)

		op, err := ctrl.CreateOrUpdate(ctx, r.Client, cronJob, func() error {
			setLabels(&cronJob.ObjectMeta)
			cronJob.Spec.Schedule = cluster.Spec.LogRotationSchedule
			cronJob.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
			cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers = []corev1.Container{
				{
					Name:    "curl",
					Image:   r.CurlContainerImage,
					Command: []string{"curl", "-sf", fmt.Sprintf("http://%s.%s:8080", podName, uniqueName(cluster))},
				},
			}
			return ctrl.SetControllerReference(cluster, cronJob, r.Scheme)
		})
		if err != nil {
			log.Error(err, "unable to create-or-update CronJob")
			return isUpdated, err
		}
		if op != controllerutil.OperationResultNone {
			log.Info("reconcile CronJob successfully", "op", op)
			isUpdated = true
		}
	}
	return isUpdated, nil
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}

func uniqueName(cluster *mocov1alpha1.MySQLCluster) string {
	return fmt.Sprintf("%s-%s", cluster.GetName(), cluster.GetUID())
}

func setLabels(om *metav1.ObjectMeta) {
	om.Labels = map[string]string{
		appNameKey:      om.Name,
		appManagedByKey: myName,
	}
}

func getMysqldContainerRequests(cluster *mocov1alpha1.MySQLCluster, resourceName corev1.ResourceName) *resource.Quantity {
	for _, c := range cluster.Spec.PodTemplate.Spec.Containers {
		if c.Name != mysqldContainerName {
			continue
		}
		r, ok := c.Resources.Requests[resourceName]
		if ok {
			return &r
		}
		r, ok = c.Resources.Limits[resourceName]
		if ok {
			return &r
		}
		return nil
	}
	return nil
}

func setCondition(conditions *[]mocov1alpha1.MySQLClusterCondition, newCondition mocov1alpha1.MySQLClusterCondition) {
	if conditions == nil {
		conditions = &[]mocov1alpha1.MySQLClusterCondition{}
	}
	current := findCondition(*conditions, newCondition.Type)
	if current == nil {
		newCondition.LastTransitionTime = metav1.NewTime(time.Now())
		*conditions = append(*conditions, newCondition)
		return
	}
	if current.Status != newCondition.Status {
		current.Status = newCondition.Status
		current.LastTransitionTime = metav1.NewTime(time.Now())
	}
	current.Reason = newCondition.Reason
	current.Message = newCondition.Message
}

func findCondition(conditions []mocov1alpha1.MySQLClusterCondition, conditionType mocov1alpha1.MySQLClusterConditionType) *mocov1alpha1.MySQLClusterCondition {
	for _, c := range conditions {
		if c.Type == conditionType {
			return &c
		}
	}
	return nil
}
